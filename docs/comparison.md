---
title: Why This Bridge
description: A factual, dated account of how genai-otel-bridge differs from a vendor's own console, in-app OpenTelemetry GenAI instrumentation, or shipping prompts to an LLM observability platform — and when to pick each.
---

# Why This Bridge

*Last reviewed: 2026-08.*

There are already several ways to see what your AI platform is doing, and for some teams one
of them is the whole answer. This page states what `genai-otel-bridge` does differently and
why, so you can decide whether it earns a process on your critical path. It describes this
codebase; it makes no claims about how anything else is implemented.

## What already exists

**The vendor's own console.** Portkey and LangSmith both have good dashboards over their own
data, and they will always know things about their platform that an API consumer does not.
They are also per-vendor, and they are not where your SLOs, alert rules or on-call runbooks
live.

**In-app OpenTelemetry GenAI instrumentation.** Tracing the calls your application actually
makes gives you causality — this request, that span, this retry — which polling an aggregate
API cannot. If what you need is a trace, instrument the app. This bridge is not a substitute
for that, and the two answer different questions.

**Shipping prompts and completions to an LLM observability platform.** The right call when
you need to inspect *what was said* — evaluating output quality, debugging a bad answer,
building a regression set. It also means prompt and completion text leaves your boundary,
which is a decision that belongs to someone other than the monitoring stack.

## Design choices specific to this bridge

**Content-free by construction, enforced as a release gate.** The bridge never requests prompt
or response bodies, and an outbound field allow/deny-list governs every emitted field. That
is not a convention or a config default: it is a three-layer model with a gate in the release
process, described in [Content Governance](governance.md). The consequence worth naming is
that this cannot leak prompt text even if a source API starts returning it — which is the
property that makes it deployable in places the alternatives are not.

**Vendor-neutral output, per-vendor sources.** LLM gateways and evaluation platforms produce
different shapes; the emitted telemetry does not. Cost, tokens, latency and errors land as
OTLP metrics and logs in the same backend as everything else you run, so an AI spend panel
sits next to the service that spent it rather than in a separate console.

**Built for the critical path.** Leader-elected single-emit, so a horizontally scaled
deployment does not double-count cost — see [High Availability](high-availability.md). It is
self-observing, and it is resilient to downstream slowness, because a telemetry pipeline that
stalls when its backend is slow becomes the incident instead of reporting it.

**Operational telemetry, deliberately narrow.** Latency, tokens, cost, errors. Not quality,
not evaluation scores over content, not a prompt archive. The narrowness is the feature: it
is what lets the content guarantee be absolute rather than conditional.

## When to pick something else

**You need to see prompts and completions.** Use a platform built for that. This bridge is
architecturally incapable of showing you them, on purpose.

**You need traces and causality.** Instrument the application with OpenTelemetry GenAI
conventions. Polled aggregates cannot reconstruct a call graph.

**One vendor, and their console is enough.** Leader election, field governance and OTLP
plumbing exist for teams consolidating several sources into one backend with alerting on top.
Below that, the vendor's dashboard is not a compromise.

## See also

- [Telemetry](telemetry.md) — every metric and log record emitted
- [Content Governance](governance.md) — the three-layer content-free model
- [High Availability](high-availability.md) — leader election and single-emit
- [Architecture](architecture.md) — how sources, governance and emitters fit together
- [Security](security.md) — credentials and what the bridge can reach
