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
			// [ext-review-14] Reject CROSS-HOST redirects. Go strips Authorization/Cookie on a
			// cross-domain redirect but FORWARDS arbitrary custom headers, so a source's vendor auth
			// header (e.g. x-portkey-api-key) would leak to a different origin. Source calls are API
			// GETs to a fixed base_url, so a legitimate redirect is same-host (scheme/path
			// canonicalisation) — anchor on the ORIGINAL request's host so the credential set there
			// can never reach another host.
			if len(via) > 0 && !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
				return fmt.Errorf("httpx: cross-host redirect to %q blocked (credentials must not leave the origin host)", req.URL.Hostname())
			}
			return cl.checkDest(req.Context(), req.URL)
		},
	}
	return cl
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
	if !allowPrivate && (ip.IsLoopback() || ip.IsPrivate()) {
		return fmt.Errorf("egress: blocked loopback/RFC-1918 %s (set allow_private to permit)", ip)
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
