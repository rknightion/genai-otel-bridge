# internal/httpx — hardened outbound HTTP client

`httpx.go` is a `*http.Client` wrapper: custom User-Agent (default UA is WAF-blocked, DESIGN §15),
per-request rate limiter, SSRF egress guard, and cross-host redirect rejection.

```go
type Config struct {
    UserAgent    string
    Timeout      time.Duration
    AllowHosts   []string      // empty ⇒ IP guard alone (no host allow-list)
    AllowPrivate bool          // permit RFC-1918/loopback (dev / in-VPC)
    Limiter      *rate.Limiter // per-request token (M6)
    Observer     Observer      // optional self-obs hook (nil ⇒ off)
}
func New(cfg Config) *Client
func (c *Client) Do(req *http.Request) (*http.Response, error)
```

## Request observer (self-obs)

`Config.Observer func(RequestInfo)`, if set, fires **once per request actually sent upstream** — after
the egress guard + limiter pass, so guard-blocked / limiter-rejected requests are NOT observed (they
aren't upstream latency). `RequestInfo` carries `{Target host, Method, StatusCode, Err, Duration}`;
`Duration` is time-to-response-headers excluding the limiter wait. Wired (in `cmd/genai-otel-bridge`) to the
selfobs `genai_otel_bridge_upstream_request_duration_seconds` histogram — httpx and selfobs stay decoupled.

## SSRF egress guard

- **Always blocked** (even with `AllowPrivate`): cloud metadata (`169.254.169.254`, `100.100.100.200`,
  `fd00:ec2::254`), CGNAT `100.64.0.0/10`, link-local (`169.254.0.0/16`, `fe80::/10`).
- **Blocked unless `AllowPrivate`:** loopback (`127.0.0.0/8`, `::1`) and RFC-1918.
- Guard runs at the dialer `Control` hook (authoritative for direct dials) **and** `checkDest` resolves
  the hostname and checks every resolved IP before the transport (covers the proxied path), fail-closed
  on resolution failure.

## Why the redirect/proxy checks exist (don't remove)

- **Cross-host redirect reject (`ext-review-14`):** `CheckRedirect` blocks a redirect to a different
  hostname. Go strips `Authorization`/`Cookie` cross-host but **forwards arbitrary custom headers** — a
  source's vendor auth (`x-portkey-api-key`) would leak to another origin. Also re-runs the host
  allow-list + IP guard on **every** hop (max 10).
- **Proxy can't be bypassed (`proxy_test.go`):** metadata block is enforced before the proxy, so
  `HTTP(S)_PROXY` can't be used to reach `169.254.169.254`.
- **Residual DNS-rebinding:** when a proxy is configured it resolves the hostname itself, so the
  rebinding race isn't fully closable at the client; mitigated by IP-literal exactness + dial-guard
  authority on direct paths. Known and documented (CP-H5).
- Rate limiter (`Limiter.Wait`) is acquired **per request before any dial** (M6) — an exhausted limiter
  never touches the network. TLS min 1.2.

Tests: guard table (`httpx_test.go`), `redirect_test.go` (cross-host auth-leak + per-hop allow-list),
`proxy_test.go` (metadata block survives proxy env).
