// SPDX-License-Identifier: AGPL-3.0-only

// Package httpx is the shared outbound HTTP client: a configurable User-Agent (default
// client UAs are WAF-blocked — DESIGN §15), a per-source rate token acquired per request
// (M6), and an SSRF egress guard (F33/H6) that default-denies link-local/metadata and,
// unless permitted, RFC-1918. The guard is enforced both at dial (the resolved IP) and on the
// destination URL of every request and redirect hop (so a configured proxy can't bypass it).
package httpx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"golang.org/x/time/rate"
)

type Config struct {
	UserAgent    string
	Timeout      time.Duration
	AllowHosts   []string // empty ⇒ any host that passes the IP guard
	AllowPrivate bool     // permit RFC-1918 (in-cluster collector / in-VPC source)
	Limiter      *rate.Limiter
	Observer     Observer // optional; per-request self-obs hook (nil ⇒ no instrumentation)
}

// RequestInfo is the outcome of one outbound request that was actually sent upstream.
type RequestInfo struct {
	Target     string        // destination host only (low-cardinality; never the full path)
	Method     string        // HTTP method
	StatusCode int           // response status, or 0 if no response was received (Err != nil)
	Err        error         // transport error, or nil
	Duration   time.Duration // time to response headers (NOT full body read), EXCLUDING the limiter wait
}

// Observer, if set on Config, is invoked exactly once per request that is actually sent upstream —
// i.e. after the egress guard and rate limiter pass. A request the guard blocks or the limiter
// rejects is NOT observed (those are our-side rejections, not upstream latency). Wired to the
// self-obs upstream-request histogram.
type Observer func(RequestInfo)

type Client struct {
	cfg Config
	hc  *http.Client
}

func New(cfg Config) *Client {
	cl := &Client{cfg: cfg}
	dialer := &net.Dialer{Timeout: cfg.Timeout, Control: guardControl(cfg.AllowPrivate)}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment, // [CP-H5] honour HTTP(S)_PROXY/NO_PROXY (corporate egress)
		DialContext:         dialer.DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	cl.hc = &http.Client{
		Timeout: cfg.Timeout,
		// otelhttp wraps the transport to emit OTel-semconv CLIENT spans (http.request.method, url.full,
		// http.response.status_code) for every upstream call, auto-parented to the active span in the
		// request context (the schedule loop.tick span). When self-tracing is disabled the global
		// TracerProvider is the no-op tracer, so this costs ~nothing. We pass a no-op MeterProvider so
		// otelhttp does NOT emit its own http.client.* metrics — the upstream duration histogram is
		// already covered by Config.Observer (genai_otel_bridge_upstream_request_duration_seconds), and we keep the
		// self-metric cardinality controlled. Trace-context is NOT propagated to vendor APIs (no global
		// propagator is set), so our internal trace IDs never leak to Portkey/LangSmith.
		Transport: otelhttp.NewTransport(tr, otelhttp.WithMeterProvider(noopmetric.NewMeterProvider())),
		// [ext-review-5] The default client follows redirects WITHOUT re-running Do's checks, so a
		// redirect could send the request to a host outside the allow-list, or (with a proxy) to a
		// metadata/RFC-1918 address the dial guard never sees. Re-validate every hop.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("httpx: stopped after 10 redirects")
			}
			if err := checkRedirectCreds(req, via); err != nil {
				return err
			}
			return cl.checkDest(req.Context(), req.URL)
		},
	}
	return cl
}

// checkRedirectCreds blocks a redirect hop that would leak the source's custom vendor auth header.
// Go's client strips Authorization/Cookie only on a CROSS-DOMAIN redirect and FORWARDS arbitrary custom
// headers otherwise, so a source's vendor auth header (e.g. x-portkey-api-key) can egress two ways:
//   - [ext-review-14] to a DIFFERENT origin host — blocked by anchoring on the ORIGINAL request's host
//     (source calls are API GETs to a fixed base_url; a legitimate redirect is same-host path/scheme
//     canonicalisation, never another origin).
//   - [#62] over CLEARTEXT on a SAME-host https→http scheme DOWNGRADE — the cross-host rule does not
//     fire (host unchanged), so anchor on the ORIGINAL request's scheme too: once the origin hop was
//     https, no later hop may be plain http (the credential must never cross the wire unencrypted).
//
// A pure function of (req, via) so the policy is unit-testable without a live TLS handshake.
func checkRedirectCreds(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	origin := via[0].URL
	if !strings.EqualFold(req.URL.Hostname(), origin.Hostname()) {
		return fmt.Errorf("httpx: cross-host redirect to %q blocked (credentials must not leave the origin host)", req.URL.Hostname())
	}
	if strings.EqualFold(origin.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
		return fmt.Errorf("httpx: redirect scheme downgrade https->%s to %q blocked (credentials must not be sent cleartext)", req.URL.Scheme, req.URL.Hostname())
	}
	return nil
}

// Do validates the destination (allow-list + egress guard), acquires the rate token (M6 — per
// request, not per Collect), sets the User-Agent, and executes the request.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	// [ext-review-4] Validate the destination BEFORE the transport. The dialer Control guard sees
	// only the dialed address — when an HTTP(S)_PROXY is configured it sees the PROXY, not the
	// destination, so a metadata/RFC-1918 destination would otherwise be forwarded to the proxy
	// unchecked. Checking here covers the initial hop uniformly with CheckRedirect.
	if err := c.checkDest(req.Context(), req.URL); err != nil {
		return nil, err
	}
	if c.cfg.Limiter != nil {
		if err := c.cfg.Limiter.Wait(req.Context()); err != nil {
			return nil, fmt.Errorf("httpx: rate limiter: %w", err)
		}
	}
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if c.cfg.Observer != nil { // observe only requests actually sent upstream (post-guard, post-limiter)
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		c.cfg.Observer(RequestInfo{Target: req.URL.Hostname(), Method: req.Method, StatusCode: code, Err: err, Duration: time.Since(start)})
	}
	return resp, err
}

// RedactURLError sanitises a transport error so a request URL's QUERY STRING can never reach a log
// line. Go's *url.Error stringifies as `Op "<full-url>": <inner>`, embedding the FULL request URL —
// and for a presigned/signed download URL the query IS a live bearer credential (X-Amz-Signature /
// X-Goog-Signature / an Azure SAS token) granting read access to the raw object. This returns an error
// whose message keeps the operation + scheme://host/path (operationally useful) but DROPS the query and
// fragment entirely, and masks any userinfo password. A non-*url.Error passes through unchanged (its
// string does not carry the request URL). nil in ⇒ nil out.
//
// The inner transport error (`ue.Err`, e.g. "dial tcp …: connection refused") does not carry the URL,
// so it is preserved (wrapped, so errors.Is/As still see it). Callers that render a download/transport
// error into a log or trace MUST route it through this first.
func RedactURLError(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	redacted := stripQuery(ue.URL)
	return fmt.Errorf("%s %q: %w", ue.Op, redacted, ue.Err)
}

// stripQuery removes the query string, fragment, and any userinfo password from a raw URL, leaving
// scheme://[user@]host/path. On a parse failure it falls back to truncating at the first '?' so a
// signature can never survive even an unparseable URL.
func stripQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		before, _, _ := strings.Cut(raw, "?")
		return before
	}
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.Redacted() // masks a userinfo password too; query already cleared
}

// ErrSnippet reads a bounded prefix of a non-2xx response body and returns it as a trimmed, single-line
// string for inclusion in an error message / trace exception. Upstream API error bodies are operational
// (e.g. a validation message naming a bad field — the 2026-06-21 runs `select` 422), NOT user content,
// so surfacing a short prefix is safe and turns an opaque "status 422" into a self-diagnosing signal.
// Bounded to 512 bytes so a large/echoed body can't bloat a log line. The caller still closes the body.
func ErrSnippet(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return strings.Join(strings.Fields(string(b)), " ") // collapse whitespace/newlines to one line
}

// checkDest enforces the host allow-list and the SSRF egress guard on a destination URL. It runs for
// the initial request AND every redirect hop, regardless of whether a proxy is in use. An IP-literal
// host is checked directly; a hostname is resolved and EVERY resolved IP is checked (fail-closed if
// resolution fails). Residual: with a proxy in use the proxy resolves the hostname itself, so a
// DNS-rebinding race between this check and the proxy's resolution can't be fully closed here — the
// IP-literal path (the metadata-SSRF case) is exact, and the dial guard remains authoritative for
// the direct path.
func (c *Client) checkDest(ctx context.Context, u *url.URL) error {
	host := strings.ToLower(u.Hostname())
	if len(c.cfg.AllowHosts) > 0 {
		if !slices.ContainsFunc(c.cfg.AllowHosts, func(h string) bool { return strings.ToLower(h) == host }) {
			return fmt.Errorf("httpx: host %q not in allow-list", u.Hostname())
		}
	}
	if host == "" {
		return fmt.Errorf("httpx: empty destination host")
	}
	if ip := net.ParseIP(host); ip != nil {
		return blockedIP(ip, c.cfg.AllowPrivate)
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("httpx: cannot resolve %q for egress check: %w", host, err)
	}
	for _, a := range addrs {
		if err := blockedIP(a.IP, c.cfg.AllowPrivate); err != nil {
			return err
		}
	}
	return nil
}

// cloudMetadataIPs are link-local/ULA/CGNAT metadata endpoints that must NEVER be reachable,
// regardless of allow_private (SSRF defence — these expose cloud credentials). [round3 MEDIUM-2]
var cloudMetadataIPs = map[string]bool{
	"169.254.169.254": true, // AWS/GCP/Azure IMDS (also link-local)
	"100.100.100.200": true, // Alibaba Cloud metadata (in CGNAT space, NOT RFC-1918)
	"fd00:ec2::254":   true, // AWS IMDS over IPv6 (ULA — would pass once allow_private is set)
}

var cgnatNet = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }() // shared/CGNAT — never egress

// blockedIP is the SSRF egress predicate. Cloud-metadata endpoints + CGNAT (100.64/10) + link-local
// are blocked ALWAYS (even with allow_private — CGNAT is not RFC-1918 so IsPrivate misses it, and the
// IPv6 IMDS ULA would otherwise be permitted once allow_private is set). Loopback + RFC-1918 are
// blocked UNLESS allow_private (dev/tests/in-pod sidecar/in-VPC host). Returns nil if dialable.
func blockedIP(ip net.IP, allowPrivate bool) error {
	if cloudMetadataIPs[ip.String()] || cgnatNet.Contains(ip) {
		return fmt.Errorf("egress: blocked cloud-metadata/CGNAT address %s", ip)
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("egress: blocked link-local/metadata %s", ip)
	}
	if !allowPrivate && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified()) {
		// [#96] The unspecified address (0.0.0.0 / :: / the IPv4-mapped ::ffff:0.0.0.0) reports
		// loopback=false/private=false, yet dialing 0.0.0.0:PORT connects to a service bound to
		// 127.0.0.1:PORT — so it is a loopback reach and must be blocked in the vendor-only posture.
		return fmt.Errorf("egress: blocked loopback/RFC-1918/unspecified %s (set allow_private to permit)", ip)
	}
	return nil
}

// guardControl is the dialer Control hook: it sees the *resolved* IP:port before connecting, so a
// hostname that resolves to a metadata/RFC-1918 address is blocked at dial (authoritative for the
// direct, non-proxied path). The destination-URL check in checkDest covers the proxied path.
func guardControl(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("egress: bad address %q: %w", address, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("egress: unresolved address %q", address)
		}
		return blockedIP(ip, allowPrivate)
	}
}
