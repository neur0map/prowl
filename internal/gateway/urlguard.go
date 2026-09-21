package gateway

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/gateway/provider"
)

// Outbound guard for user-supplied custom-provider base URLs (SSRF), ported
// from FreeLLMAPI (server/src/lib/url-guard.ts, #440).
//
// A custom endpoint's base_url is the one place a dashboard user chooses where
// the gateway sends a request carrying a stored credential. Left unchecked it
// is an SSRF vector - point it at a cloud metadata service and a probe (or the
// ~5-minute health pass) fetches IAM credentials. The guard lives in the vault
// package, not the HTTP layer, so it can sit at the one chokepoint every writer
// passes through - KeyVault.Add - rather than being re-applied per route where
// a single missed call site reopens the hole.
//
// Policy, calibrated for a local-first app that points at Ollama/llama.cpp on
// localhost or the LAN: cloud-metadata and link-local addresses are ALWAYS
// blocked; loopback and private ranges are allowed unless
// PROWL_BLOCK_PRIVATE_PROVIDER_URLS is set; everything else is allowed. A
// hostname that will not resolve now is allowed (a LAN device not yet up); the
// connect-time Control hook in the HTTP layer re-checks the address actually
// dialled, so that concession is safe against DNS rebinding.

// providerMetadataIPs are the exact IMDS addresses outside the link-local range
// (url-guard.ts:40-53).
var providerMetadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"), // AWS/GCP/Azure IMDS
	net.ParseIP("100.100.100.200"), // Alibaba Cloud IMDS
	net.ParseIP("192.0.0.192"),     // Oracle Cloud legacy IMDS
	net.ParseIP("fd00:ec2::254"),   // AWS IMDSv2 IPv6
}

// ProviderBlockPrivate reports whether loopback/private targets are refused. Off
// by default so local inference servers keep working; recommended on for a
// gateway whose dashboard is exposed beyond the host.
func ProviderBlockPrivate() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PROWL_BLOCK_PRIVATE_PROVIDER_URLS"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// parseProviderHostIP parses a URL hostname as an IP, including the bare
// decimal-integer form (http://2852039166/ → 169.254.169.254) that is a known
// SSRF bypass of a naive string check.
func parseProviderHostIP(host string) net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return ip
	}
	if n, err := strconv.ParseUint(host, 10, 32); err == nil {
		return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	return nil
}

// ClassifyProviderIP buckets an address into the guard's policy classes. It is
// exported so the HTTP layer's connect-time Control hook can re-run the exact
// same verdict on the address actually dialled.
func ClassifyProviderIP(ip net.IP) string {
	for _, m := range providerMetadataIPs {
		if m != nil && ip.Equal(m) {
			return "metadata"
		}
	}
	// CGNAT 100.64.0.0/10 holds Alibaba's IMDS and is never a published LLM
	// endpoint (url-guard.ts:64-66).
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return "private"
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "link-local"
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return "loopback"
	}
	if ip.IsPrivate() {
		return "private"
	}
	return "public"
}

// init wires the connect-time SSRF verdict into the provider registry's shared
// transport. The provider package builds that transport but must not import
// gateway (import cycle), so the policy is injected here: the same
// ClassifyProviderIP buckets and ProviderBlockPrivate switch AssessProviderURL
// applies at save time are re-run on the address the routed inference transport
// is actually dialling, so a host that rebinds to a private/link-local/metadata
// address after the save-time check is refused before the credential is sent.
func init() {
	provider.DialGuard = func(address string) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return nil
		}
		switch ClassifyProviderIP(ip) {
		case "metadata":
			return fmt.Errorf("refusing connection to cloud metadata address %s", host)
		case "link-local":
			return fmt.Errorf("refusing connection to link-local address %s", host)
		case "loopback", "private":
			if ProviderBlockPrivate() {
				return fmt.Errorf("refusing connection to private address %s", host)
			}
		}
		return nil
	}
}

// AssessProviderURL returns whether an outbound custom-provider URL is safe to
// store and contact, and a human reason when it is not. It never errors on bad
// input - a malformed URL is simply refused.
func AssessProviderURL(rawURL string) (bool, string) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return false, "not a valid URL"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false, "unsupported protocol " + u.Scheme
	}
	host := strings.ToLower(u.Hostname())
	if host == "metadata.google.internal" || host == "metadata.goog" {
		return false, "cloud metadata endpoints are not reachable through custom providers"
	}

	var addrs []net.IP
	if ip := parseProviderHostIP(host); ip != nil {
		addrs = []net.IP{ip}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			// Unresolvable now may resolve later; the connect-time Control hook
			// re-checks the address actually dialled.
			return true, ""
		}
		for _, a := range ips {
			addrs = append(addrs, a.IP)
		}
	}

	blockPrivate := ProviderBlockPrivate()
	for _, ip := range addrs {
		switch ClassifyProviderIP(ip) {
		case "metadata":
			return false, fmt.Sprintf("resolves to a cloud metadata address (%s)", ip)
		case "link-local":
			return false, fmt.Sprintf("resolves to a link-local address (%s)", ip)
		case "loopback", "private":
			if blockPrivate {
				return false, fmt.Sprintf("resolves to a private address (%s) and PROWL_BLOCK_PRIVATE_PROVIDER_URLS is set", ip)
			}
		}
	}
	return true, ""
}
