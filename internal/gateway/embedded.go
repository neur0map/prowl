package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PingChallengeHeader carries a fresh nonce used to authenticate the process
// behind a loopback gateway port without sending either gateway credential.
const PingChallengeHeader = "X-Prowl-Ping-Challenge"

// PingProof binds a liveness challenge to the listener port. Binding the port
// prevents a hostile local listener from relaying the challenge to a genuine
// Prowl gateway on another port and replaying its answer.
func PingProof(token string, port int, challenge string) string {
	if token == "" || port <= 0 || challenge == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = fmt.Fprintf(mac, "%d\n%s", port, challenge)
	return hex.EncodeToString(mac.Sum(nil))
}

func newPingChallenge() (string, bool) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", false
	}
	return hex.EncodeToString(nonce[:]), true
}

// probeGateway checks that whatever holds the port is a Prowl gateway. When a
// token is available, the ungated liveness response must prove knowledge of it
// without the client ever transmitting that credential to an untrusted port.
func probeGateway(ctx context.Context, port int, token string) bool {
	_, ok := probeGatewayPID(ctx, port, token)
	return ok
}

// probeGatewayPID returns the process identity asserted by the gateway holding
// port. Stop operations compare it with the durable PID record before sending
// a signal, preventing stale PID reuse from targeting an unrelated process.
func probeGatewayPID(ctx context.Context, port int, token string) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://127.0.0.1:%d/api/ping", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false
	}
	challenge := ""
	if token != "" {
		var ok bool
		challenge, ok = newPingChallenge()
		if !ok {
			return 0, false
		}
		req.Header.Set(PingChallengeHeader, challenge)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var body struct {
		Status string `json:"status"`
		PID    int    `json:"pid"`
		Proof  string `json:"proof"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return 0, false
	}
	if body.Status != "ok" {
		return 0, false
	}
	if token != "" {
		expected := PingProof(token, port, challenge)
		if !hmac.Equal([]byte(body.Proof), []byte(expected)) {
			return 0, false
		}
	}
	return body.PID, true
}

// Running reports the dashboard URL when a Prowl gateway already holds the
// port, so a second invocation can point at it instead of failing to bind.
func Running(ctx context.Context, port int, token string) (string, bool) {
	if port == 0 {
		port = DefaultPort
	}
	if !probeGateway(ctx, port, token) {
		return "", false
	}
	return fmt.Sprintf("http://127.0.0.1:%d/", port), true
}
