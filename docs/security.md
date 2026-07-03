---
title: Security
description: Security model for genai-otel-bridge — SSRF guard, secret handling, content-free design, and vulnerability reporting.
---

# Security

## Content-free design

genai-otel-bridge **never requests prompt text, completion text, message bodies, or any LLM
inference content** from the platform APIs it polls. It collects only operational signals:
request counts, latencies, token counts, cost, error rates, and status codes.

This is enforced at three layers:

1. **Source strip** — a default-deny allow-list in each source package drops every field not
   on the explicit list before any value enters the processing pipeline.
2. **Source.Guard content denylist** — a defence-in-depth backstop at the composition root
   denies the `AbsoluteNeverDenyKeys` floor regardless of any opt-in:
   `gen_ai.*`, `input.value`, `output.value`, `request`, `response`, `inputs`, `outputs`,
   `messages`, `metadata`, `portkeyHeaders`.
3. **Conformance gate tests** — `TestLogsExportContentLeakConformanceGate` and
   `TestLangsmithRunsContentLeakConformanceGate` are release gate tests in `internal/app`.
   They assert the full wired pipeline produces zero content fields in the outbound OTLP
   payload and must remain green on every release.

See [Content Governance](./governance.md) for the full model.

---

## SSRF egress guard

Outbound HTTP requests go through `internal/httpx`, a hardened client that enforces an
egress guard at the dialer level:

**Always blocked** (even if `AllowPrivate` is set):

- Cloud metadata endpoints: `169.254.169.254`, `100.100.100.200`, `fd00:ec2::254`
- CGNAT range: `100.64.0.0/10`
- Link-local: `169.254.0.0/16`, `fe80::/10`

**Blocked unless `AllowPrivate` is true** (dev/in-VPC use):

- Loopback: `127.0.0.0/8`, `::1`
- RFC-1918 private ranges

The guard runs at two points: the dialer `Control` hook (authoritative for direct dials) and
a pre-dial hostname resolution check (`checkDest`) that resolves every IP before the
transport. The guard fails closed on DNS resolution failure.

The Portkey `logs_export` loop adds a second layer for signed-URL downloads: the download
URL is validated against `settings.signed_url_allow_hosts` with an exact host match before
any fetch, independent of the dialer guard.

---

## Cross-host redirect block

`internal/httpx` rejects redirects to a different hostname. Go strips `Authorization` and
`Cookie` headers on cross-host redirects but **forwards arbitrary custom headers** — a
source's vendor auth token (such as `x-portkey-api-key`) would otherwise leak to a different
origin. The redirect check also re-runs the host allow-list and IP guard on every hop.

---

## Secret handling

Credentials are never stored in git. The config file supports two secret reference forms:

```yaml
auth:
  header: x-portkey-api-key
  value: ${PORTKEY_API_KEY}       # resolved from the environment
  # value: file:/run/secrets/key  # ...or resolved from a file path at load time
```

Plaintext credentials in the config file are accepted for dev use but should be avoided in
production. Use Kubernetes Secrets with ExternalSecret / SecretStore for production
deployments, or inject credentials via environment variables.

Credentials are **never logged** — the bridge redacts auth values in all log output and
self-telemetry.

---

## Kubernetes network policy

The Helm chart deploys a default-deny NetworkPolicy. Egress is whitelisted for:

- DNS (CoreDNS pods in `kube-system`, by label selector — not a CIDR, which would break on
  some CNIs)
- Kubernetes API server (`apiServerCIDR` — required for leader-election Lease and
  checkpoint ConfigMap)
- OTLP endpoint (port 443, configurable CIDR)
- Source APIs (port 443, configurable CIDR)

Ingress is default-deny except the health port (8080) from any source (required for kubelet
probes). The pod cannot receive traffic from the product plane.

!!! note "EKS"
    Network policy enforcement on EKS requires the VPC-CNI network-policy feature (or
    Calico) to be enabled.

---

## TLS

All outbound HTTP connections require TLS 1.2 as a minimum. The `internal/httpx` client
sets `TLSMinVersion: tls.VersionTLS12`. Connections to plaintext endpoints (HTTP, not HTTPS)
require explicit opt-in via the `emit.*.otlp.allow_insecure` config flag, which should only
be used for in-cluster collector deployments where the traffic does not leave the cluster.

---

## RBAC (Kubernetes)

The pod's ServiceAccount is bound to a **namespace-scoped Role** (not a ClusterRole):

- `coordination.k8s.io/leases`: `get`, `create`, `update` on the `genai-otel-bridge-leader`
  Lease only — the pod cannot touch any other Lease.
- `configmaps`: `get`, `create`, `update` on the `genai-otel-bridge-checkpoints` ConfigMap only
  — the pod cannot read or modify its own `genai-otel-bridge-config` ConfigMap.
- `delete` is not granted to the pod's ServiceAccount. The post-delete cleanup hook uses a
  separate ephemeral ServiceAccount that only exists during uninstall.

---

## License

genai-otel-bridge is released under the **GNU Affero General Public License v3.0 only**
(`AGPL-3.0-only`). Every source file carries the SPDX identifier:

```go
// SPDX-License-Identifier: AGPL-3.0-only
```

Third-party dependencies retain their own upstream licenses. See
[`LICENSING.md`](https://github.com/rknightion/genai-otel-bridge/blob/main/LICENSING.md) and the
[full license text](https://github.com/rknightion/genai-otel-bridge/blob/main/LICENSE).

---

## Reporting a vulnerability

Do not open a public issue for security vulnerabilities. Report privately via GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
(**Security → Report a vulnerability** on the repository).

Reports that demonstrate a way to make the service emit inference content, leak credentials,
or bypass the egress or SSRF guards are particularly in scope.

See [`SECURITY.md`](https://github.com/rknightion/genai-otel-bridge/blob/main/SECURITY.md)
for the full policy.

---

## See also

- [Content Governance](./governance.md) — field allow/deny model
- [High Availability](./high-availability.md) — RBAC and Kubernetes resource model
- [Configuration](./configuration.md) — secret reference syntax
