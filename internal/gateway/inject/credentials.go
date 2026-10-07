package inject

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

const unifiedCredentialPrefix = "prowlag-"

var (
	unifiedCredentialPattern = regexp.MustCompile(unifiedCredentialPrefix + `[0-9a-f]{48}`)
	baseURLPattern           = regexp.MustCompile(`https?://[^\s"'\\]+`)
)

// RefreshOptions carries the two credentials that remain valid while stale
// harness entries are rewritten with the current unified key.
type RefreshOptions struct {
	Home       string
	LocalToken string
	UnifiedKey string
}

// CredentialStale reports whether a recorded target was authored with an old
// unified key. Machine-local tokens and content with no unified credential are
// not stale.
func CredentialStale(target Target, localToken, currentUnifiedKey string) bool {
	for _, credential := range targetUnifiedCredentials(target) {
		if credential != localToken && credential != currentUnifiedKey {
			return true
		}
	}
	return false
}

// RefreshStale rewrites intact targets that still carry an old unified key.
// It uses the normal Apply transaction so ownership and rollback guarantees are
// identical to an explicit reinjection. Diverged targets are left untouched.
func RefreshStale(opts RefreshOptions) ([]string, error) {
	var refreshed []string
	err := withInjectLock(opts.Home, func() error {
		record, err := loadRecord(opts.Home)
		if err != nil {
			return err
		}
		harnesses := make([]string, 0, len(record.Targets))
		var refreshErr error
		applied := make(map[string]bool)
		for harness := range record.Targets {
			harnesses = append(harnesses, harness)
		}
		sort.Strings(harnesses)
		for _, harness := range harnesses {
			target := record.Targets[harness]
			if target.Harness == "" {
				target.Harness = harness
			}
			if !CredentialStale(target, opts.LocalToken, opts.UnifiedKey) ||
				!staleCredentialEntriesIntact(target, opts.LocalToken, opts.UnifiedKey) {
				continue
			}
			baseURL, ok := recordedBaseURL(target)
			if !ok {
				continue
			}
			applyHarness := harness
			if harness == prowlAliasHarness {
				applyHarness = prowlLegacyHarness
			}
			if applied[applyHarness] {
				continue
			}
			if _, err := writerFor(applyHarness); err != nil {
				continue
			}
			active, _ := Active(opts.Home, applyHarness)
			if _, err := applyLocked(Options{
				Home:     opts.Home,
				BaseURL:  baseURL,
				Token:    opts.UnifiedKey,
				Activate: active,
				Models:   RoutingModels(),
			}, applyHarness); err != nil {
				refreshErr = errors.Join(refreshErr, fmt.Errorf("%s: %w", harness, err))
				continue
			}
			applied[applyHarness] = true
			refreshed = append(refreshed, applyHarness)
		}
		return refreshErr
	})
	return refreshed, err
}

func targetUnifiedCredentials(target Target) []string {
	var credentials []string
	for _, entry := range target.Ledger {
		for _, authored := range entryAuthoredContent(entry) {
			credentials = append(credentials, unifiedCredentials(authored)...)
		}
	}
	return credentials
}

func unifiedCredentials(content string) []string {
	matches := unifiedCredentialPattern.FindAllStringIndex(content, -1)
	credentials := make([]string, 0, len(matches))
	for _, match := range matches {
		if match[0] > 0 && credentialWordByte(content[match[0]-1]) {
			continue
		}
		if match[1] < len(content) && credentialWordByte(content[match[1]]) {
			continue
		}
		credentials = append(credentials, content[match[0]:match[1]])
	}
	return credentials
}

func credentialWordByte(b byte) bool {
	return b == '_' || b == '-' ||
		b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func entryAuthoredContent(entry writtenEntry) []string {
	contents := make([]string, 0, 5+len(entry.Nested)+len(entry.Single))
	contents = appendNonEmpty(contents, entry.CreatedContent, entry.YamlAuthored,
		entry.TomlAuthored, entry.TomlVal, entry.YamlValue)
	for _, authored := range entry.Nested {
		contents = appendNonEmpty(contents, authored)
	}
	for _, authored := range entry.Single {
		contents = appendNonEmpty(contents, authored)
	}
	return contents
}

func appendNonEmpty(values []string, candidates ...string) []string {
	for _, candidate := range candidates {
		if candidate != "" {
			values = append(values, candidate)
		}
	}
	return values
}

func staleCredentialEntriesIntact(target Target, localToken, currentUnifiedKey string) bool {
	found := false
	for _, entry := range target.Ledger {
		entryTarget := Target{Ledger: []writtenEntry{entry}}
		if CredentialStale(entryTarget, localToken, currentUnifiedKey) {
			found = true
		}
		if entry.Activation || !providerEntry(entry) {
			continue
		}
		if !providerEntryIntact(entry) {
			return false
		}
	}
	return found
}

func providerEntry(entry writtenEntry) bool {
	return entry.CreatedFile || entry.EnvFile || entry.YamlBlock != "" ||
		entry.TomlTable != "" || entry.Container != "" || len(entry.Single) > 0
}

func providerEntryIntact(entry writtenEntry) bool {
	info, err := os.Lstat(entry.Path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	raw, err := os.ReadFile(entry.Path)
	if err != nil {
		return false
	}
	if entry.CreatedFile && entry.CreatedContent != "" {
		return string(raw) == entry.CreatedContent
	}
	if entry.EnvFile {
		return entry.CreatedContent != "" && string(raw) == entry.CreatedContent
	}
	if entry.YamlBlock != "" {
		start, end, ok := ompBlockRangeFor(string(raw), entry.YamlBlock)
		return ok && authoredBlockIntact(string(raw[start:end]), entry.YamlAuthored)
	}
	if entry.TomlTable != "" {
		start, end, ok := tomlTableRange(string(raw), entry.TomlTable)
		return ok && authoredBlockIntact(string(raw[start:end]), entry.TomlAuthored)
	}
	obj, err := parseObject(raw)
	if err != nil {
		return false
	}
	if entry.Container != "" {
		container, ok := obj.get(entry.Container)
		if !ok || !objectMatchesNested(container, entry.Nested) {
			return false
		}
	}
	for key, authored := range entry.Single {
		current, ok := obj.get(key)
		if !ok || !jsonEq(current, json.RawMessage(authored)) {
			return false
		}
	}
	return entry.Container != "" || len(entry.Single) > 0
}

func recordedBaseURL(target Target) (string, bool) {
	for _, entry := range target.Ledger {
		for _, authored := range entryAuthoredContent(entry) {
			baseURL := baseURLPattern.FindString(authored)
			if baseURL == "" {
				continue
			}
			if target.Harness == "claude" {
				// Claude records the gateway root; injections before 0.17 recorded
				// the /v1 form. Either way Apply expects the /v1 base.
				baseURL = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1") + "/v1"
			}
			return baseURL, true
		}
	}
	return "", false
}
