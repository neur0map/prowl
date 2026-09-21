package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/cespare/xxhash/v2"
	"github.com/google/uuid"

	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
)

const (
	claudeCodeVersion           = "2.1.257"
	claudeCodeSystemInstruction = "You are Claude Code, Anthropic's official CLI for Claude."
	claudeToolPrefix            = "_"
	claudeBillingHeaderPrefix   = "x-anthropic-billing-header:"
	cchSeed                     = 0x4d659218e32a3268
	cchPlaceholderText          = "cch=00000"
)

var claudeCodeBetas = []string{
	"claude-code-20250219",
	oauthBeta,
	"interleaved-thinking-2025-05-14",
	"thinking-token-count-2026-05-13",
	"context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05",
	"mid-conversation-system-2026-04-07",
	"effort-2025-11-24",
	"fallback-credit-2026-06-01",
}

var anthropicBuiltinToolNames = map[string]struct{}{
	"web_search": {}, "code_execution": {}, "text_editor": {}, "computer": {},
}

// IsOAuthToken distinguishes Claude subscription credentials from API keys.
func IsOAuthToken(key string) bool { return strings.HasPrefix(key, "sk-ant-oat") }

// ApplyAuthHeaders applies the credential form accepted by Anthropic's API and
// model-discovery endpoints. It never sends both credential headers.
func ApplyAuthHeaders(header http.Header, key string) {
	header.Del("Authorization")
	header.Del("X-Api-Key")
	header.Set("anthropic-version", "2023-06-01")
	if IsOAuthToken(key) {
		header.Set("Authorization", "Bearer "+key)
		header.Set("anthropic-beta", oauthBeta)
		header.Set("anthropic-dangerous-direct-browser-access", "true")
		return
	}
	header.Set("X-Api-Key", key)
}

// OAuthClient wraps a client with the Claude Code subscription contract. API
// keys use the original client and are shaped only by ApplyAuthHeaders.
func OAuthClient(base *http.Client, key string) *http.Client {
	if !IsOAuthToken(key) {
		return base
	}
	if base == nil {
		base = http.DefaultClient
	}
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copy := *base
	copy.Transport = &Transport{Base: transport, Token: &oauth.Token{AccessToken: key}}
	return &copy
}

// applyClaudeCodeHeaders sets the subscription wire contract Anthropic accepts
// for an OAuth bearer: the Claude Code identity, version and beta headers, and
// the direct-browser-access flag. RoundTrip and the informational usage probe
// share it so the two request shapes never drift apart.
func applyClaudeCodeHeaders(header http.Header, token string) {
	header.Set("Authorization", "Bearer "+token)
	header.Set("User-Agent", "claude-cli/"+claudeCodeVersion+" (external, cli)")
	header.Set("anthropic-version", "2023-06-01")
	header.Set("anthropic-beta", mergeBetas(header.Get("anthropic-beta")))
	header.Set("anthropic-dangerous-direct-browser-access", "true")
	header.Set("x-app", "cli")
	for key, value := range claudeCodeStaticHeaders() {
		header.Set(key, value)
	}
}

// usageEndpoint is the private Claude Code statusline usage report. It is a var
// only so a test can point it at a stub; production always dials Anthropic.
var usageEndpoint = BaseURL + "/api/oauth/usage"

// usageHTTPClient dials the usage report as an ordinary GET. The message-shaping
// RoundTrip is deliberately uninvolved: this endpoint returns no tool calls to
// restore and is not POST /messages.
var usageHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
	// An OAuth bearer must never chase a provider redirect onto another host.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// UsageWindow is one rate-limit bucket: Utilization is a 0-100 percentage and
// ResetsAt the wall clock the bucket refills.
type UsageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// UsageLimit is one row of the live generic limits[] array. Kind is
// session|weekly_all|weekly_scoped; Percent is a 0-100 utilization; a
// weekly_scoped row names its model family in Scope.Model.DisplayName.
//
// is_active is deliberately NOT modelled: live payloads mark only the
// currently-binding limit active (an account pinned at a 100% Fable cap reports
// its shared weekly row as is_active:false), so it signals severity ranking,
// not bucket existence - filtering on it hides real utilization.
type UsageLimit struct {
	Kind     string           `json:"kind"`
	Percent  *float64         `json:"percent"`
	ResetsAt string           `json:"resets_at"`
	Scope    *usageLimitScope `json:"scope"`
}

type usageLimitScope struct {
	Model *usageLimitModel `json:"model"`
}

type usageLimitModel struct {
	DisplayName string `json:"display_name"`
}

func (l UsageLimit) displayName() string {
	if l.Scope != nil && l.Scope.Model != nil {
		return strings.TrimSpace(l.Scope.Model.DisplayName)
	}
	return ""
}

// reported mirrors the reference: a row counts only when it names a kind and
// carries at least a percent or a reset.
func (l UsageLimit) reported() bool {
	return strings.TrimSpace(l.Kind) != "" && (l.Percent != nil || strings.TrimSpace(l.ResetsAt) != "")
}

func (l UsageLimit) window() *UsageWindow {
	w := &UsageWindow{ResetsAt: l.ResetsAt}
	if l.Percent != nil {
		w.Utilization = *l.Percent
	}
	return w
}

// ScopedWindow is one per-model-family weekly cap with the family's display
// name; there may be several (Opus, Sonnet, Fable, Mythos).
type ScopedWindow struct {
	Label       string
	Utilization float64
	ResetsAt    string
}

// Usage is the subscription usage report from GET /api/oauth/usage. Anthropic
// keeps the legacy account-wide five_hour/seven_day buckets populated; the
// per-model weekly caps (seven_day_opus/seven_day_sonnet) are now permanently
// null and arrive instead as generic limits[] entries. Every bucket pointer is
// nil when the plan does not report it.
type Usage struct {
	FiveHour       *UsageWindow `json:"five_hour"`
	SevenDay       *UsageWindow `json:"seven_day"`
	SevenDayOpus   *UsageWindow `json:"seven_day_opus"`
	SevenDaySonnet *UsageWindow `json:"seven_day_sonnet"`
	Limits         []UsageLimit `json:"limits"`
}

// Session5h is the five-hour rolling window: the legacy five_hour bucket, else
// the generic limits[] session row.
func (u *Usage) Session5h() *UsageWindow {
	if u == nil {
		return nil
	}
	if u.FiveHour != nil {
		return u.FiveHour
	}
	return u.limitWindow("session")
}

// Weekly is the seven-day account-wide window: the legacy seven_day bucket, else
// the generic limits[] weekly_all row.
func (u *Usage) Weekly() *UsageWindow {
	if u == nil {
		return nil
	}
	if u.SevenDay != nil {
		return u.SevenDay
	}
	return u.limitWindow("weekly_all")
}

func (u *Usage) limitWindow(kind string) *UsageWindow {
	for _, l := range u.Limits {
		if l.Kind == kind && l.reported() {
			return l.window()
		}
	}
	return nil
}

// ScopedWeekly lists every per-model-family weekly cap: the legacy Opus/Sonnet
// buckets (retained though Anthropic now sends them null) followed by each
// generic weekly_scoped row named by its model display name, deduplicated.
func (u *Usage) ScopedWeekly() []ScopedWindow {
	if u == nil {
		return nil
	}
	var out []ScopedWindow
	seen := map[string]bool{}
	add := func(label string, w *UsageWindow) {
		key := strings.ToLower(strings.TrimSpace(label))
		if w == nil || key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, ScopedWindow{Label: label, Utilization: w.Utilization, ResetsAt: w.ResetsAt})
	}
	add("Opus", u.SevenDayOpus)
	add("Sonnet", u.SevenDaySonnet)
	for _, l := range u.Limits {
		if l.Kind != "weekly_scoped" || !l.reported() {
			continue
		}
		add(l.displayName(), l.window())
	}
	return out
}

// FetchUsage reads the subscription's rate-window utilization. It is an
// informational GET under the Claude Code OAuth contract; it mutates nothing
// and returns the parsed windows or a plain error - never the bearer.
func FetchUsage(ctx context.Context, token string) (*Usage, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("claude usage lookup needs an OAuth access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageEndpoint, nil)
	if err != nil {
		return nil, err
	}
	applyClaudeCodeHeaders(req.Header, token)
	req.Header.Set("Accept", "application/json")
	resp, err := usageHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude usage request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read claude usage: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("claude usage endpoint returned %d", resp.StatusCode)
	}
	// The endpoint answers 200 with an error envelope when the beta header is
	// missing or the token is stale; that is a failure, not empty usage.
	var envelope struct {
		Type  string `json:"type"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Type == "error" {
		if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
			return nil, fmt.Errorf("claude rejected the usage request: %s", envelope.Error.Message)
		}
		return nil, fmt.Errorf("claude rejected the usage request")
	}
	var usage Usage
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, fmt.Errorf("parse claude usage: %w", err)
	}
	return &usage, nil
}

// Transport presents OAuth inference as Claude Code while preserving the
// caller's prompt and tool ownership. Anthropic rejects subscription bearers
// sent as ordinary API keys; the headers, identity blocks, attestation, and
// tool-name round trip below are all part of that wire contract.
type Transport struct {
	Base      http.RoundTripper
	Token     *oauth.Token
	once      sync.Once
	sessionID string
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.Token == nil {
		return base.RoundTrip(req)
	}
	r := req.Clone(req.Context())
	r.Header.Del("X-Api-Key")
	r.Header.Del("Authorization")
	if r.URL.Scheme != "https" || !strings.EqualFold(r.URL.Hostname(), "api.anthropic.com") {
		return base.RoundTrip(r)
	}
	applyClaudeCodeHeaders(r.Header, t.Token.AccessToken)
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
		return base.RoundTrip(r)
	}
	if r.Body == nil {
		return nil, fmt.Errorf("claude request has no body")
	}
	data, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read Claude request: %w", err)
	}
	sessionID := r.Header.Get("x-session-id")
	if sessionID != "" {
		sessionID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(sessionID)).String()
	} else {
		t.once.Do(func() { t.sessionID = uuid.NewString() })
		sessionID = t.sessionID
	}
	r.Header.Set("x-claude-code-session-id", sessionID)
	data, names, err := claudeRequestBody(data)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.Header.Del("Content-Length")
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	resp, err := base.RoundTrip(r)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || len(names) == 0 {
		return resp, err
	}
	contentType := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(contentType, "text/event-stream"):
		resp.Body = &toolNameStream{source: resp.Body, reader: bufio.NewReader(resp.Body), names: names}
	case strings.HasPrefix(contentType, "application/json"):
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		body, err = restoreToolNames(body, names)
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
	default:
		return resp, nil
	}
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return resp, nil
}

func claudeRequestBody(data []byte) ([]byte, map[string]string, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(data, &body) != nil || body == nil {
		return nil, nil, fmt.Errorf("claude request must be a JSON object")
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(body["messages"], &messages) != nil {
		return nil, nil, fmt.Errorf("claude request messages must be an array")
	}
	firstUser := ""
	for _, message := range messages {
		if jsonString(message["role"]) != "user" {
			continue
		}
		if json.Unmarshal(message["content"], &firstUser) != nil {
			var blocks []map[string]json.RawMessage
			_ = json.Unmarshal(message["content"], &blocks)
			for _, block := range blocks {
				if jsonString(block["type"]) == "text" {
					firstUser = jsonString(block["text"])
					break
				}
			}
		}
		break
	}
	textBlock := func(text string) json.RawMessage {
		block, _ := json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{"text", text})
		return block
	}
	system := []json.RawMessage{textBlock(createClaudeBillingHeader(firstUser)), textBlock(claudeCodeSystemInstruction)}
	if raw := body["system"]; len(raw) > 0 && string(raw) != "null" {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			if text != "" {
				system = append(system, textBlock(text))
			}
		} else {
			var blocks []json.RawMessage
			if json.Unmarshal(raw, &blocks) != nil {
				return nil, nil, fmt.Errorf("invalid Claude system prompt")
			}
			system = append(system, blocks...)
		}
	}
	body["system"], _ = json.Marshal(system)

	names := make(map[string]string)
	prefixName := func(object map[string]json.RawMessage) {
		name := jsonString(object["name"])
		if name == "" {
			return
		}
		wire := applyClaudeToolPrefix(name)
		if wire == name {
			return
		}
		names[wire] = name
		object["name"], _ = json.Marshal(wire)
	}
	if raw := body["tools"]; len(raw) > 0 && string(raw) != "null" {
		var tools []map[string]json.RawMessage
		if json.Unmarshal(raw, &tools) != nil {
			return nil, nil, fmt.Errorf("invalid Claude tools")
		}
		for _, tool := range tools {
			prefixName(tool)
		}
		body["tools"], _ = json.Marshal(tools)
	}
	for _, message := range messages {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(message["content"], &blocks) != nil {
			continue
		}
		changed := false
		for _, block := range blocks {
			if jsonString(block["type"]) == "tool_use" {
				prefixName(block)
				changed = true
			}
		}
		if changed {
			message["content"], _ = json.Marshal(blocks)
		}
	}
	body["messages"], _ = json.Marshal(messages)
	if raw := body["tool_choice"]; len(raw) > 0 && string(raw) != "null" {
		var choice map[string]json.RawMessage
		if json.Unmarshal(raw, &choice) != nil {
			return nil, nil, fmt.Errorf("invalid Claude tool choice")
		}
		if jsonString(choice["type"]) == "tool" {
			prefixName(choice)
			body["tool_choice"], _ = json.Marshal(choice)
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	if !patchCch(data) {
		return nil, nil, fmt.Errorf("could not attest Claude request")
	}
	return data, names, nil
}

func mergeBetas(existing string) string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(claudeCodeBetas)+4)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	for _, value := range claudeCodeBetas {
		add(value)
	}
	for _, value := range strings.Split(existing, ",") {
		add(value)
	}
	return strings.Join(out, ",")
}

func claudeCodeStaticHeaders() map[string]string {
	osName := map[string]string{"darwin": "MacOS", "windows": "Windows", "linux": "Linux", "freebsd": "FreeBSD"}[runtime.GOOS]
	if osName == "" {
		osName = "Other::" + runtime.GOOS
	}
	arch := map[string]string{"amd64": "x64", "arm64": "arm64", "386": "x86"}[runtime.GOARCH]
	if arch == "" {
		arch = "other::" + runtime.GOARCH
	}
	return map[string]string{
		"X-Stainless-Arch": arch, "X-Stainless-Lang": "js", "X-Stainless-OS": osName,
		"X-Stainless-Package-Version": claudeCodeSDKVersion, "X-Stainless-Retry-Count": "0",
		"X-Stainless-Runtime": "node", "X-Stainless-Runtime-Version": "v26.3.0",
		"X-Stainless-Timeout": "600",
	}
}

func createClaudeBillingHeader(firstUser string) string {
	units := utf16.Encode([]rune(firstUser))
	selected := []uint16{'0', '0', '0'}
	for i, index := range [...]int{4, 7, 20} {
		if index < len(units) {
			selected[i] = units[index]
		}
	}
	seed := string(utf16.Decode(selected))
	sum := sha256.Sum256([]byte("59cf53e54c78" + seed + claudeCodeVersion))
	suffix := hex.EncodeToString(sum[:2])[:3]
	return fmt.Sprintf("%s cc_version=%s.%s; cc_entrypoint=cli; %s;", claudeBillingHeaderPrefix, claudeCodeVersion, suffix, cchPlaceholderText)
}

func applyClaudeToolPrefix(name string) string {
	if _, ok := anthropicBuiltinToolNames[strings.ToLower(name)]; ok {
		return name
	}
	return claudeToolPrefix + name
}

var (
	cchPlaceholder      = []byte(cchPlaceholderText)
	billingSystemMarker = []byte(`"system":[{"type":"text","text":"` + claudeBillingHeaderPrefix)
)

func patchCch(body []byte) bool {
	marker := bytes.Index(body, billingSystemMarker)
	if marker < 0 {
		return false
	}
	start := marker + len(billingSystemMarker)
	end := bytes.IndexByte(body[start:], '"')
	if end < 0 {
		return false
	}
	rel := bytes.Index(body[start:start+end], cchPlaceholder)
	if rel < 0 {
		return false
	}
	digest := xxhash.NewWithSeed(cchSeed)
	_, _ = digest.Write(body)
	hash := digest.Sum64()
	const digits = "0123456789abcdef"
	for i := 4; i >= 0; i-- {
		body[start+rel+4+i] = digits[hash&0xf]
		hash >>= 4
	}
	return true
}

func jsonString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func restoreToolNames(data []byte, names map[string]string) ([]byte, error) {
	var event map[string]json.RawMessage
	if json.Unmarshal(data, &event) != nil {
		return nil, fmt.Errorf("invalid Claude response JSON")
	}
	changed := false
	restore := func(block map[string]json.RawMessage) bool {
		if jsonString(block["type"]) != "tool_use" {
			return false
		}
		name, ok := names[jsonString(block["name"])]
		if !ok {
			return false
		}
		block["name"], _ = json.Marshal(name)
		return true
	}
	if jsonString(event["type"]) == "content_block_start" {
		var block map[string]json.RawMessage
		if json.Unmarshal(event["content_block"], &block) == nil && restore(block) {
			event["content_block"], _ = json.Marshal(block)
			changed = true
		}
	} else if jsonString(event["type"]) == "message" {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(event["content"], &blocks) == nil {
			for _, block := range blocks {
				changed = restore(block) || changed
			}
			if changed {
				event["content"], _ = json.Marshal(blocks)
			}
		}
	}
	if !changed {
		return data, nil
	}
	return json.Marshal(event)
}

type toolNameStream struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	names   map[string]string
	pending []byte
	err     error
}

func (s *toolNameStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		line, err := s.reader.ReadBytes('\n')
		s.err = err
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(line[len("data:"):])
			if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
				converted, convertErr := restoreToolNames(payload, s.names)
				if convertErr != nil {
					s.err = convertErr
					return 0, convertErr
				}
				line = append([]byte("data: "), converted...)
				line = append(line, '\n')
			}
		}
		s.pending = line
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *toolNameStream) Close() error { return s.source.Close() }
