# internal/httpx

The shared outbound HTTP client wrapping `*http.Client`: configurable User-Agent (Go's default UA is
WAF-blocked, DESIGN section 15), a per-request rate token, an SSRF egress guard, and redirect
credential protection. TLS minimum is 1.2.

## Request observer

`Config.Observer` fires exactly once per request actually sent upstream, after the egress guard and
the limiter pass, so guard-blocked and limiter-rejected requests are not observed: they are our-side
rejections, not upstream latency. `RequestInfo.Duration` is time to response headers, excluding the
limiter wait and the body read. `Target` is the destination host only, never the path, to keep the
label low-cardinality. The composition root wires it to
`genai_otel_bridge_upstream_request_duration_seconds`, so httpx and selfobs stay decoupled.

## SSRF egress guard

- Always blocked, even with `AllowPrivate`: cloud metadata (`169.254.169.254`, `100.100.100.200`,
  `fd00:ec2::254`), CGNAT `100.64.0.0/10`, and link-local. CGNAT is not RFC-1918, so `IsPrivate`
  misses it and it needs its own check.
- Blocked unless `AllowPrivate`: loopback, RFC-1918 and the unspecified address.
- The guard runs both at the dialer `Control` hook, authoritative for direct dials, and in
  `checkDest`, which resolves the hostname and checks every resolved IP before the transport so the
  proxied path is covered too. A resolution failure fails closed.
- The limiter is acquired per request before any dial, so an exhausted limiter never touches the
  network.

## Why the redirect and proxy checks exist

- Go strips `Authorization` and `Cookie` on a cross-domain redirect but forwards arbitrary custom
  headers, so a source's vendor auth header (`x-portkey-api-key`) would leak. `CheckRedirect`
  therefore anchors on the original request's host and its scheme: a hop to a different origin is
  blocked, and once the origin hop was https no later hop may be plain http. The host allow-list and
  IP guard re-run on every hop, up to 10.
- The metadata block is enforced before the proxy, so `HTTP(S)_PROXY` cannot be used to reach
  `169.254.169.254`.
- Residual DNS-rebinding is known and accepted: with a proxy configured the proxy resolves the
  hostname itself, so the race is not closable at the client. It is mitigated by IP-literal exactness
  and dial-guard authority on direct paths.
