# Multi-tenancy for genai-otel-bridge

*   **Status:** Draft. Work after the test harness depends on open questions Q1 and Q2.
*   **Date:** 2026-11-12

## Overview

genai-otel-bridge polls AI-platform APIs and emits operational telemetry as metrics and logs over
OTLP, the OpenTelemetry protocol. Today one process serves one team: it has one OTLP destination,
one identity, one governance budget and one set of checkpoints. A team that wants telemetry from
its LLM gateway gets its own install.

This design lets one install serve many teams. A central observability team runs it. Each team
hands over an API key for its workspace in Portkey, an LLM gateway, and that workspace's telemetry
is sent to that team's own OTLP destination under that team's own credentials. Portkey is the
first source. The expected size is 10 to 20 teams at launch and about 250 later.

This document specifies the launch: one leader-elected install that runs many isolated pipelines.
It does not specify shards (a second install with a different list of tenants). It checks only
that nothing decided here has to be undone for them. One install has a ceiling, because a restart
pauses every tenant in it and because every checkpoint save goes through one object. The working
ceiling is set at about 25 tenants. That figure is an estimate. Questions Q3 and Q5 test it,
taking one pipeline with two loops per tenant. The size of 250 is a forecast and is not tested
here.

### How the bridge works today

A reader needs these to follow the rest.

*   A **source** is one vendor integration. It runs one or more **loops**. Each loop wakes on a
    **cadence**, pulls a bounded time **window** forward from its **watermark** (the time up to
    which it has emitted), and produces a batch of metric samples or log records. Portkey's
    `analytics` and `groups` loops produce metrics only. Its `logs_export` loop produces logs.
*   A loop emits a time bucket only once it is older than **settle** (about ten minutes), because
    the vendor's API can still revise a recent bucket. A loop never reads further back than its
    lookback (`max_backfill` for `analytics`, the window span for `groups`; 90 minutes in the
    chart's default config), because the metrics backend refuses older samples. Time lost beyond
    that is counted as skipped.
*   The **guard** sits between a loop and the emitter. It drops any sample or record that has a
    label key not on an allow-list or that carries a denied content field, and it caps the number
    of distinct label sets per metric name (the cardinality budget).
*   The **emitter** encodes a batch as OTLP and posts it to one endpoint. Every payload carries
    three resource attributes, here called the identity: `service.name` (always
    `genai-otel-bridge`), `service.namespace` and `deployment.environment.name`.
*   A watermark is saved to the **checkpoint store** (one Kubernetes ConfigMap) only after its
    batch has been emitted. Replicas elect a leader through a Kubernetes **lease**, and only the
    leader runs loops.
*   Three mechanisms together mean that a failover leaves no gap and no duplicate in the backend,
    provided the new leader starts within the lookback. They are called the **gap-free
    mechanisms** below. A bucket is emitted once, after settle. Encoding is byte-identical for the
    same input, so a new leader that replays a window sends exactly what the old leader sent and
    the backend treats it as a repeat. A checkpoint save is refused if it moves the watermark
    backward or comes from an older leader. That refusal is called the checkpoint fence.
*   **Self-telemetry** is the bridge's metrics about itself, such as `window_lag` (now minus the
    watermark). It goes to its own OTLP endpoint when one is configured, and otherwise to the
    product endpoint, under a separate identity whose namespace ends in `-meta`. Telemetry
    republished from a source is called product telemetry.
*   The binary's `main` package builds one emitter and one self-metrics object and passes them to
    the **composition root**, the package that wires sources, loops, the guard and the scheduler
    together.

### Terms

These are used with one meaning throughout. The new code and the new config keys use only these
words. Vendor words such as "workspace" appear only inside a source package.

*   **Install.** One Helm release of the chart: one leader and its optional standby.
*   **Version.** One published build of the binary and chart.
*   **Operator.** A member of the central team that runs the install. A Kubernetes operator (a
    controller that manages installs) is always written out in full.
*   **Tenant.** The unit an operator adds or removes, usually one team. It has an id that is not
    meant to change. It owns its destinations and targets, and cannot refer to another tenant's.
*   **Target.** One source API key plus one scope id.
*   **Scope id.** The name of the one thing the API key is expected to see. Core treats it as an
    opaque string. For Portkey it is the workspace slug. Core does not filter by it and, in this
    version, the Portkey package sends no scope parameter: the API key alone decides what a
    request returns. The scope id is what the scope assertion compares against.
*   **Destination.** One OTLP endpoint with the instance id and token it expects as Basic-auth
    user and password. For Grafana Cloud the instance id is the stack's instance id.
*   **Stack.** The backend a destination points at, such as a Grafana Cloud stack. Two
    destinations point at the same stack when their instance ids are equal and their endpoints
    are equal after lower-casing the host and dropping a trailing slash. "Tenant" in this document
    never means a Grafana Cloud, Mimir or Loki tenant.
*   **Identity.** The three resource attributes on emitted telemetry. Each pipeline has one for
    product telemetry. The install has one for self-telemetry.
*   **Pipeline.** One target at run time: its loops, its guard, its emitter, its self-metric
    labels and its checkpoints. It is the unit of isolation. Its id is `<tenant>.<target>`.
*   **Legacy shape and tenant shape.** The two forms a config file can take: today's `sources`
    list with one `emit.telemetry`, or the new `tenants` map.
*   **Config check.** The binary's `-validate-config` mode, which validates a config file without
    secrets and exits.

## The Problem

A central team cannot run today's binary for many teams without one install per team.

*   **One install per team uses too many pods.** The author's guide for this design is about 20
    pods for 50 teams, which is 0.4 pods per team. It is a requirement for the design, not a
    measured platform limit. One install per team is one pod per team with a single replica and
    two with a standby.
*   **One process cannot serve two teams today.** The OTLP destination and the identity are global.
    Startup fails when two loops declare the same metric name. Self-metrics are labelled by loop
    name only, so two sources with an `analytics` loop overwrite each other's lag gauge. The
    guard's budget is shared.
*   **Putting many teams in one process creates a new failure: one team's data delivered to
    another team's stack.** Nothing in the binary guards against it, because until now it could not
    happen. The most likely cause is not mis-wiring inside the binary. It is an API key that sees
    something other than what the config says: the wrong key pasted for a tenant, or a key with
    wider access than expected.

The solution is to make the pipeline the unit of isolation. Each pipeline is assembled in one
place from one row of a resolved plan, and carries its own guard, emitter, self-metric labels and
checkpoint keys. After every collect a pipeline checks what its API key can see, and queues the
batch only if the check passes. The existing single-team config keeps working unchanged.

## Goals

Each goal states how it is checked.

*   **G1. One install serves many tenants, each to its own destination with its own credentials.**
    Test: the k3d end-to-end test runs three tenants and each fake stack receives only its own
    tenant's records.
*   **G2. Four controls guard against one tenant's data reaching another tenant's destination.**
    They are: a scope assertion at the source that fails closed, one place
    where a pipeline is assembled, a release gate that is shown to fail on seeded faults, and
    run-time evidence an operator can query. Section 5.1 and the Risks section state what they do
    not catch. Check: the release gate in section 5.3 passes, including its seeded faults.
*   **G3. One pipeline's run-time failure does not stop or delay any other pipeline.** Tests: with
    one destination returning 503 for 10 minutes, every other pipeline's `window_lag` stays within
    settle plus two cadences. A pipeline with a revoked API key or a scope mismatch does not
    advance, a tenant with an unreadable secret is not built, and in each case every other
    pipeline runs.
*   **G4. Every pipeline is individually alertable.** Lag, skips and degraded state carry the
    pipeline's id. Test: two pipelines with the same loop name produce two distinct `window_lag`
    series.
*   **G5. The three gap-free mechanisms work unchanged inside each pipeline.** Test: the existing
    acceptance tests pass with their assertions unmodified, and the same scenarios pass when run
    with three pipelines.
*   **G6. An existing single-team install upgrades with no config change.** Test: golden tests pin
    the checkpoint keys for the three checkpoint backends (ConfigMap, DynamoDB and file), the
    emitted OTLP bytes and the self-metric label sets of the legacy shape. An upgrade test starts
    the new binary on a checkpoint store written by the current version.
*   **G7. The launch fits the pod guide.** Check: the launch install runs at most two pods, and
    question Q5 passes.
*   **G8. Formats that reach durable state or tenant stacks do not change after the first
    tenant-shape version.** These are the checkpoint key format, the pipeline id format, the name
    of the pipeline attribute on product telemetry, the self-metric labels `tenant` and
    `pipeline`, and the tenant config keys. Test: golden tests in that version pin each one.
*   **G9. New config keys are vendor-neutral.** Test: a unit test fails when any key in the tenant
    config schema contains a word on a fixed list (`portkey`, `workspace`, `langsmith`). New core
    code is held to the repository's existing "Decoupled" rule in review.

## Non-Goals

*   **LangSmith in the tenant shape.** The tenant config must not rule it out, but this design
    does not specify it.
*   **API keys that are meant to cover several scopes.** One target has one scope.
*   **Log records in the tenant shape.** A loop that emits log records is rejected until question
    Q7 shows an API key cannot export another scope's logs.
*   **Per-API-key use-case labels in the tenant shape.** The legacy `api_key_use_cases` setting is
    not offered on a target in this version.
*   **Discovery of scopes.** No pipeline is created from a listing.
*   **Fan-out from one collection to several destinations.** A second destination for the same
    scope is a second target, which makes its own requests and keeps its own watermark.
*   **Pattern matching on targets, and routing by anything finer than a scope.**
*   **Adding or removing pipelines while the process runs.** Config is read once. A change is a
    rollout.
*   **Recovering a tenant without a restart after its secret changes.**
*   **Shards.** Running a second install in one namespace, a tenant registry, moving a tenant
    between installs, and a checkpoint fence that holds across two leases. A second install in its
    own namespace works with today's chart and is the fallback until that design exists.
*   **A Kubernetes operator or a custom resource.**
*   **Multi-tenant operation on ECS (the AWS container service) with DynamoDB.** Single-team ECS
    keeps working.
*   **A shared rate limiter, a partitioned checkpoint store, a process-wide memory budget, and
    per-tenant governance settings.** None has a measured need at the launch size.
*   **Delivering a tenant's health signals into the tenant's own stack.** A one-page data contract
    for tenant teams is in scope in its place.
*   **A self-service API or UI.**

## Open Questions

The implementation is staged as six numbered changes, listed under "Order of work" at the end of
this document. Change 1 is a tests-only harness. Each question has a pass criterion and the
change or event it must be answered before.

| Id | Question | Pass | Needed before |
| --- | --- | --- | --- |
| Q1 | What is Portkey's request budget, and is it per API key, per organisation or per client IP address? The repository records "about 10 requests per 10 seconds" for Portkey in one place and for LangSmith in another. This design makes 9 requests per tenant per minute at a 60-second cadence (5 graph requests, 2 group requests, 2 assertions) and 10 per tenant just after a restart. | The investigation reports the largest tenant count the budget supports at a 60-second and at a 5-minute cadence, including the requests after a restart. The count at a 5-minute cadence is at least 20. If only that count reaches 20, the launch default cadence becomes 5 minutes. If the budget is shared across API keys and supports fewer than 20 at a 5-minute cadence, this design is stopped and reassessed: no number of installs raises a shared budget. | Change 2 |
| Q2 | Would Portkey's own OpenTelemetry export replace polling? It would only if all four hold: (a) it is available on the deployment in use; (b) it carries cost, tokens, latency and errors per workspace; (c) it carries no prompt or completion content; (d) it loses no more than the lookback of 90 minutes when the receiver is down. | Polling stays: at least one of the four does not hold. If all four hold, this design is stopped and reassessed. | Change 2 |
| Q6 | Does Portkey's workspace dimension show everything its data endpoints show? The property needed is: for every graph or group response received at a time t1 over any range inside a loop's lookback, every workspace that contributed to it appears, with a non-empty slug, on the first page of a workspace-dimension response over the seven days ending at any later time t2. Also: is a workspace slug unique across organisations, or only inside one? | The property holds in repeated trials, for a workspace key and for an organisation key, with new traffic injected into a second workspace and both endpoints polled every few seconds, and when the key is being rate-limited. If it does not hold, the tenant shape does not ship. | Change 4 |
| Q3 | What are the memory and CPU of a leader and a standby with 1, 20 and 50 pipelines, each at its full cardinality budget? | The leader's working set at 20 pipelines fits the chart's memory limit with half to spare. | Change 6 |
| Q5 | What checkpoint save rate does one install sustain at 50, 150 and 300 loops, against a real Kubernetes API server, and are lease renewals delayed by checkpoint traffic? | 50 loops each save a checkpoint once per 60-second cadence for 30 minutes, no loop's tick starts late, and no lease renewal is delayed. If saves are the limit, the first response is Alternative L, not a partitioned store. If renewals are delayed, the lease gets its own Kubernetes client. | Change 6 |
| Q4 | On each destination's stack, do the three identity attributes become metric labels, and is the pipeline attribute kept and searchable? | Yes for both. | Each tenant's onboarding |
| Q7 | Can a workspace API key export another workspace's logs, and does an export record carry its workspace? | A foreign workspace id is refused, or records carry a workspace the package can check. | Log records in the tenant shape |
| Q8 | Does the launch run a hot standby? A standby takes the lease about 15 seconds after the leader stops. It does not add correctness, because recovery is from checkpoints either way. It doubles the pod count. | A decision by the author. The design works with one replica or two. | Launch |

## Critical User Journeys (CUJs)

### CUJ 1: Add a tenant

*   **Given**: a running install with several tenants, and a new team's workspace API key and
    OTLP destination credentials stored as a Kubernetes Secret.
*   **When**: the operator adds the tenant to the chart values, runs the config check, and rolls
    the install.
*   **Then**: the config check prints the plan table, which shows where every target is sent, so
    the change can be reviewed as a diff. After the rollout the new tenant's pipeline asserts its
    scope. If the workspace has had traffic in the last seven days the assertion passes, and its
    metrics appear in its stack once the first buckets are older than settle, about ten minutes
    later. If it has had none, the pipeline reports "unverified", emits nothing, and starts by
    itself on the next tick after traffic arrives. Every other pipeline pauses for one or two
    leadership handovers and resumes from its checkpoints with no counted skips.

### CUJ 2: A target's API key is revoked

*   **Given**: a running target whose API key is then revoked at the vendor.
*   **When**: that pipeline's next request to the vendor returns 401 or 403.
*   **Then**: that pipeline's loops stop advancing. An auth-error signal carrying the tenant and
    pipeline ids fires. No other pipeline's lag changes. When the operator replaces the key in the
    Secret and restarts the pods, the pipeline resumes from its watermark and reads back up to
    its lookback.

### CUJ 3: A tenant's destination is down

*   **Given**: one destination returns 503 for 10 minutes.
*   **When**: emits to it fail and retry.
*   **Then**: only the pipelines that send to that destination show rising lag. Every other
    pipeline's lag stays within settle plus two cadences. When the destination recovers, those
    pipelines re-pull from their watermarks and emit the missed windows.

### CUJ 4: The wrong API key is configured for a target

*   **Given**: a target whose API key belongs to a different workspace, and that workspace has had
    traffic in the last seven days.
*   **When**: the pipeline starts.
*   **Then**: the scope assertion returns a mismatch before anything is collected. Nothing is
    emitted for that pipeline and no watermark advances. Its loops are retried every ten minutes
    and a signal names the tenant, the pipeline and the reason. Every other pipeline runs.

The assertion sees which workspaces had traffic, not what a key is allowed to access. Section 5.1
states what it does not catch.

### CUJ 5: Upgrade an existing single-team install

*   **Given**: an install on the current version with the legacy shape.
*   **When**: the operator upgrades the image with no config change.
*   **Then**: the binary reads the existing checkpoints under the same keys and emits
    byte-identical payloads. No watermark resets and no series changes identity. A rollback to the
    previous image works.

### CUJ 6: On-call checks where a tenant's data went

*   **Given**: a suspicion that tenant A's data was delivered to the wrong stack.
*   **When**: the operator looks at the bridge's self-telemetry and at the receiving stack.
*   **Then**: a log line written each time a replica becomes leader gives the plan hash, which the
    operator matches to the plan table printed in CI. A counter by pipeline shows how many records
    each pipeline emitted. Product telemetry in a stack carries the id of the pipeline that sent
    it, so a stack can be searched for another tenant's pipeline id.

## Technical Plan (Architecture)

### 1. Shape of the change

Today `main` builds one emitter and one self-metrics object, and the composition root builds one
guard. The same three are handed to every loop runner. After this change the composition root
builds a plan from config and then one pipeline per row of the plan.

```text
config (legacy shape or tenant shape)
        |
        v
resolve ----> plan: one row per target
        |     (tenant, target, source, scope, destination, identity, loops)
        |     printed by the config check; its hash is logged when a replica becomes leader
        v
for each row: assemble one pipeline       <-- the only place a loop meets an emitter
        |
        +-- source (vendor package)   own HTTP client, own rate limiter
        +-- scope gate                asked by the scheduler before and after each collect
        +-- guard                     own allow-list, deny map and cardinality budget
        +-- emitter                   own destination credentials, own identity
        +-- telemetry hooks           tenant and pipeline on every self-metric
        +-- loop runners              one per loop
        v
scheduler (one, leader only) --- coordinator (one lease) --- checkpoint store (one ConfigMap)
```

This change alters none of the interfaces the repository marks as frozen: the model types,
`source.Source`, `source.Loop`, `emit.Emitter`, `checkpoint.Checkpointer` and
`coordinate.Coordinator`. The loop runner's emit and commit logic, the encoder and the checkpoint
store, where the three gap-free mechanisms live, are not changed.

What does change in existing code:

*   The composition root's `Build` function takes two factories in place of one emitter and one
    metrics object. The telemetry factory makes the telemetry hooks for a tenant and a pipeline.
    The emitter factory makes an emitter from a destination, an identity and those hooks. The
    pairing of a plan row with its credentials and identity is in the composition root, where the
    release gate can reach it.
*   The scheduler's per-loop record gains that loop's telemetry hooks, its pipeline id and its
    pipeline's scope gate. The scheduler asks the gate before and after each collect.
*   The loop runner's constructor gains one argument carrying the pipeline id for log lines.
*   Every place that walks the legacy `sources` list (the startup summary, the liveness
    threshold, the content opt-in collection) reads the plan.

### 2. Config model

A config has the tenant shape when its `tenants` map has at least one entry. Then `sources` must
be empty and `emit.telemetry` must have no endpoint, instance id or token. A config with no
tenants has the legacy shape, and `defaults` must be absent. Everything else in this section
applies to the tenant shape only.

```yaml
identity:                             # required; the install's own identity, for self-telemetry
  service_namespace: genai-otel-bridge
  deployment_environment: prod

emit:
  self:                               # required; where self-telemetry goes
    otlp: { endpoint: ..., instance_id: ..., token: ... }

defaults:
  portkey:                            # keyed by source type
    loops:                            # used by a target for each loop name it does not set itself
      analytics:
        enabled: true
        cadence: 60s
        window: 50m
        bucket_settle: 10m
        bootstrap_lookback: 50m
        max_backfill: 90m
        metric_prefix: portkey_api
        graphs: [requests, cost, tokens, latency, errors]
      groups:
        enabled: true
        cadence: 60s

tenants:                              # map keyed by tenant id
  team-a:
    identity:                         # required; stamped on this tenant's product telemetry
      service_namespace: team-a-llm
      deployment_environment: prod
    destinations:                     # map keyed by name, local to this tenant
      main:
        otlp:
          endpoint: https://otlp.example/otlp
          instance_id: "000000"       # plain config, reviewable
          token_file: otlp-token      # a file name in this tenant's secrets directory
    targets:                          # map keyed by name, local to this tenant
      prod:
        source:
          type: portkey
          base_url: https://api.portkey.ai/v1
          auth: { header: x-portkey-api-key, value_file: source-key }
          rate_limit: { rps: 1, burst: 3 }
        scope: example-workspace-slug
        destination: main
        # identity: {...}             # omit to use the tenant's identity
        # loops: {...}                # omit to use defaults.<type>.loops
```

The `governance`, `queue`, `ha`, `log` and `selfobs` blocks keep their meaning and apply to the
whole install. There is no per-tenant form of them in this version: the cardinality budget, the
label-key opt-in list and the rate cap are the same for every tenant.

Rules. Each is fatal. The phase that checks each one is in section 3.

*   Tenant ids and the names of destinations and targets match
    `[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?`. A rename is not blocked. A renamed tenant or target
    starts new watermarks and leaves its old checkpoint keys behind, and the documentation says
    so.
*   `tenants`, `destinations` and `targets` are maps, never lists, because Helm replaces a list
    when values files are layered and merges a map.
*   A target names one destination inside its own tenant. There is no syntax for referring to
    another tenant's objects, no pattern and no precedence.
*   `scope` is required for every target.
*   A tenant's `identity` is required, with both fields. It does not fall back to the install's
    top-level `identity`.
*   A target with an `identity` block uses that block, whole, in place of the tenant's.
*   A target that sets a block for a loop name uses that block, whole, in place of the block of
    that name in `defaults.<type>.loops`. A loop name the target does not set uses its block from
    `defaults.<type>.loops`. No field is inherited from one block into another. Every loop block,
    wherever it is written, must be complete: after the loader applies the defaults it applies to
    a legacy loop block today (settle), it must pass the validation a legacy loop block passes.
    The same holds for a tenant source and the legacy defaults for its rate limit.
*   The tenant `source` block has its own schema: `type`, `base_url`, `auth`, `rate_limit` and
    the existing optional HTTP settings. It has no `enabled`, `source_instance` or `loops` key.
    The existing rules that a source URL and an OTLP endpoint must be https apply. A destination
    has no setting that allows a plain-text endpoint.
*   A tenant's secrets are named by `token_file` on each destination and `value_file` on each
    target. Each is a bare file name, with no path separator, read from that tenant's own secrets
    directory: `/var/run/secrets/genai-otel-bridge/tenants/<tenant id>/`. One tenant's block
    cannot name another tenant's secret file.
*   No value under `tenants` or `defaults` may be an `${ENV}` reference or a `file:` reference.
    Everything there is literal. This keeps secrets out of the config tree, and it means the
    config check and the running leader see the same plan (section 3).
*   `emit.self` is required. In the tenant shape self-telemetry never falls back to a tenant's
    destination.
*   A destination must have an `instance_id` and a `token_file`. A receiver that takes no
    credentials has no boundary between tenants.
*   Two tenants may not share a stack. A stack's credentials are one boundary, and inside it only
    the identity would separate two teams' data.
*   Two targets of one tenant that share a stack must differ in identity. Otherwise both would
    write the same series at the same timestamps and the stack would reject one of them.
*   No target may share both a stack and an identity with the install's self-telemetry.
*   The source type must support the scope assertion (section 5.1).
*   A loop that emits log records is rejected.
*   `ha.coordinator` must be `lease` and `ha.checkpoint` must be `configmap`.

### 3. The resolved plan and the three phases

A new package turns a loaded config into a plan: a sorted list of rows, one per target. A row holds
the tenant id, the target name, the source settings, the scope id, the destination, the identity,
the loop configs and the pipeline id.

*   The **plan table** is the plan rendered as text with no secrets: tenant, target, source type,
    source URL, scope, destination name, endpoint, instance id, identity and enabled loops.
*   The **plan hash** is the SHA-256 of the plan table. In the tenant shape every field in the
    table is a literal, and CI and the running leader use the same image, so both render the same
    text. In the legacy shape a field may be an `${ENV}` reference, which the config check
    replaces with a placeholder when it is unset, so the two hashes match only when CI has the
    same environment.
*   The config check prints the plan table and the plan hash. A replica logs the plan hash and
    the row count each time it becomes leader.
*   The plan is built once. Nothing consults it at emit time.

The legacy shape resolves to one row per enabled `sources` entry, in config order. Every legacy
row has the same destination (`emit.telemetry`) and the same identity (the top-level `identity`),
an empty tenant id, an empty pipeline id and no scope. The plan table shows its
`source_instance` in the pipeline column.

Rules are checked in three phases:

| Phase | Where | Checks |
| --- | --- | --- |
| Load | Config package | Shape, ids and names, required keys, complete loop blocks, the https rules, the form of the secret file names, no references under `tenants` or `defaults`, `ha` values. |
| Resolve | Plan package | References between a target and its destination, the two stack-sharing rules, the self-telemetry rule. |
| Assemble | Composition root | Rules that need a built source and its loops: scope assertion support, no loop that emits log records, no two loops in a group declaring one metric name (section 4). |

The config check runs the load and resolve phases and never reads a tenant's secret files. The
assemble-phase rules are found when a pod starts. That is safe because the chart requires a new
pod to become ready before an old one is removed (section 11): a pod that fails to assemble never
becomes ready, and the old pods keep running.

### 4. Pipeline assembly

The composition root assembles pipelines from the plan in three passes.

1.  **Build sources.** For each tenant, read its secret files through a reader the caller
    supplies. For each row, ask the telemetry factory for the row's telemetry hooks, build the
    source hooks from them, and build the source through the existing registry. A source takes
    its hooks when it is built, so the hooks must exist first.
2.  **Build guards.** Group the rows that share a stack and an identity. For each group, build
    one guard from the loops of that group's sources, and run the existing series-ownership check
    on them. In the tenant shape the guard reports through the telemetry hooks of the group's one
    pipeline. In the legacy shape it reports through the unlabelled hooks.
3.  **Pair loops with emitters.** For each row, one function asks the emitter factory for an
    emitter with the row's destination credentials, identity and telemetry hooks, builds the
    scope gate, and builds one loop runner per loop. This function is the only code that puts a
    loop and an emitter together, and both come from the same row.

The guard has to come after the sources, because today's guard is built from the loops it will
police.

A plan row carries the existing source config struct, filled in by the plan package: the source
settings, the loop blocks, a source instance (`mt.<tenant>.<target>` in the tenant shape) and the
scope id. The scope id is a new field on that struct which a config file cannot set. Pass 1 adds
the secret values to a copy of the row. The plan table never shows them.

What each level owns:

| Thing | Owned by | Note |
| --- | --- | --- |
| Source HTTP client and rate limiter | pipeline | As today, one per source entry. |
| Scope gate | pipeline | One assertion state covers all the pipeline's loops. |
| Guard: allow-list, deny map, seen-values set, budget | group | In the tenant shape a group is one pipeline, by the rules of section 2. In the legacy shape every row shares one stack and one identity, so there is one guard, as today. Its allow-list is the vendor packages' keys, the install-wide opt-in list, and the opt-ins of that group's own loops. |
| Emitter and its request observers | pipeline | Identity is fixed when an emitter is built. |
| Source hooks (capability, incomplete data, auth error, revised bucket, upstream latency) | pipeline | Rebuilt per pipeline from its telemetry hooks. Today they are built once. |
| Loop runner, queue, watermark | loop | As today. |
| Self-metric labels, log attributes, log rate-limit keys | pipeline | Section 8. |
| Scheduler, lease, checkpoint store, liveness, self-telemetry exporter | install | None is keyed by tenant. |
| The Go HTTP transport under every emitter | install | Emitters share Go's default connection pool today and still do. It holds connections, not tenant data, and has no per-host limit that one slow stack could exhaust. |

The existing series-ownership check fails startup when two loops declare the same metric name,
because two writers of one series at the same timestamps make the backend reject one of them. It
now runs per group, not per process. Two tenants can both emit `portkey_api_requests` to
different stacks. The legacy shape is one group, so its check is the same as today.

### 5. Controls against cross-tenant delivery

#### 5.1 Scope assertion at the source

The most likely wrong delivery is an API key that sees something other than what the config says.
A check on the binary's wiring cannot catch it, because the wiring is right and the data is wrong.

A source may implement one optional capability:

```go
// ScopeAsserter is implemented by a Source that can report what its API key can see.
// Matched means: every record the source's loops could have returned, from any collect
// that finished before this call, over any range inside their lookback, belongs to the
// configured scope.
type ScopeAsserter interface {
    AssertScope(ctx context.Context) (ScopeResult, error)
}
// ScopeResult is Matched, Mismatch or Unverifiable. An error means the call failed.
```

The Portkey package implements it with a call it already makes for its existing optional check: it
reads the workspace dimension of the analytics API over the last seven days.

*   `Matched`: exactly one workspace was returned and its slug is the configured scope id.
*   `Mismatch`: another workspace, more than one, or a row with no workspace.
*   `Unverifiable`: no workspace had traffic in that window.
*   A failed call: any non-200 status, a response the vendor marks as quota-limited, a response
    that cannot be decoded, or a workspace value that cannot be read. None of these is ever
    treated as a result.

When a scope id is set, the Portkey package also refuses at start a loop whose lookback is longer
than six days, which leaves a day's margin inside that window, and refuses the legacy
`settings.expected_workspace` setting.

The policy is one object per pipeline, the **scope gate**. It lives in the source package beside
the capability, as the guard does. It holds one state for the pipeline, in memory: no result,
matched, unverified or mismatched. It makes one assertion call at a time. The scheduler resets
every gate once each time a replica becomes leader, before any loop starts, and again when the
replica stops leading. So a new leader starts with no result, and a replica that is not leading
reports none. The scheduler asks the gate at two points in every tick of every loop in the
pipeline.

**Before the collect**, after the scheduler's existing check that the loop has no batch in
flight. If the state is matched, the tick goes on. If it is unverified or mismatched and the gate
called the assertion less than a minute ago, the state stands. Otherwise the gate calls the
assertion.

**After the collect, before the batch is queued.** The gate calls the assertion again, every time.
A batch is queued only if this call returns `Matched`. The one exception is a batch that carries
nothing and does not move the watermark. It needs no call and is queued as it is today.

What happens for each outcome, at either point:

*   `Matched`: the state is matched and the tick goes on.
*   `Mismatch`: the state is mismatched. Any collected batch is dropped. The scheduler puts that
    loop into the existing degraded state, so it is ticked every ten minutes instead of at its
    cadence. Each of the pipeline's loops goes degraded on its own next tick. Only a later
    `Matched` result ends a mismatch; an `Unverifiable` result does not. That happens when the
    API key is corrected and the pods restarted, or when the other workspace's traffic has aged
    out of the seven days. The loop then collects again, and its next successful checkpoint save
    clears its degraded state, as it does today.
*   `Unverifiable`, when not mismatched: the state is unverified. Any collected batch is dropped.
    Nothing is queued, no watermark moves and no error is counted. The loop keeps its cadence, so
    the gate asserts again on the next tick, at most once a minute.
*   A failed call: the state does not change. Any collected batch is dropped. The tick is handled
    like a failed collect today: lag is recorded, the error is counted and logged against the loop
    whose tick made the call, and the loop retries at its cadence with nothing advanced. The
    assertion reports a 401 or 403 as a distinct kind of error, and the scheduler then raises the
    auth-error signal for that loop. The assertion does not raise it itself.

When the gate ends a tick before the collect because the pipeline is unverified or mismatched,
the scheduler records no lag and counts no unreadable time for that tick. When the pipeline
resumes, the time that has become older than its lookback is counted once as unreadable, under
the existing skip reason, as it is after any outage.

**What this proves and what it rests on.**

*   A key for the wrong single workspace, with traffic in the last seven days, is caught before
    anything is collected. This does not depend on timing.
*   A key with wider access is a different case. A batch that contains a second workspace's data
    is dropped only if the assertion made after that collect lists the second workspace. That is
    the property in question Q6. It is an observed behaviour of the vendor's API, not a documented
    one, and nothing in the binary can detect it ceasing to hold. The tenant shape does not ship
    unless Q6 passes, and Q6 is run again for each version that changes the Portkey package.
*   Two cases return `Matched` when they should not. A wider key whose other workspaces have had
    no traffic for seven days returns `Matched` until one of them gets traffic, and is then
    caught subject to the point above. A key from another Portkey organisation whose workspace
    has the same slug returns `Matched`, if slugs are unique only inside an organisation.
*   The main defence against a wider key is not this check. It is that a team issues a key bound
    to one workspace (see Risks).

**Cost.** Each collect is followed by one more request. A tenant with the two default loops at a
60-second cadence makes 9 requests a minute in place of 7. A failed assertion discards a
completed collect, so those requests are made again on the next tick. A waiting pipeline makes at
most one request a minute.

**What the operator documentation states.**

*   A tenant onboarded before its workspace has traffic emits nothing until traffic arrives.
*   A tenant whose workspace has had no traffic for seven days becomes unverified and stops
    emitting, including the zero-valued samples it emitted while idle. When traffic returns, the
    pipeline reads back at most its lookback. Earlier time is counted as skipped. That time held
    only zeros.
*   Until a mismatch ends nothing is emitted for that pipeline, and data older than the lookback
    when it ends is lost and counted. The mismatch alert pages, so this loss is not silent.

In the tenant shape the gate is mandatory. The legacy shape keeps its existing optional per-loop
check (`settings.expected_workspace`), untouched, because G6 requires the legacy path not to
change. Two mechanisms therefore exist in the code. At most one is active for any pipeline: a
legacy row has no scope id and so no gate, and a row with a scope id refuses the legacy setting.

#### 5.2 One place of assembly

A pipeline's emitter is created in pass 3 of assembly, from the same plan row as its source, and
is never looked up afterwards. Nothing at run time chooses a destination.

This does not detect an assembly bug that pairs a loop with the wrong emitter. The release gate
covers that.

#### 5.3 The release gate

One acceptance test, run under Go's race detector. It follows the pattern of the repository's
existing content-leak gate, an acceptance test that must pass before any version that can emit
logs.

*   At least three tenants are built through the real composition root, from plan rows, with the
    production emitter factory and real OTLP emitters. The fake source is an HTTP server, as in
    the existing acceptance tests.
*   Every destination uses the **same endpoint URL** and differs only in instance id and token. A
    test that gives each tenant its own fake server would pass a bug that sends the right URL with
    the wrong credential.
*   A fake source returns a distinct marker for each API key and scope. A fake OTLP receiver
    records payloads by credential. The check is: every payload received under a credential
    carries only that tenant's marker and that tenant's pipeline id.
*   The check must hold through each of these: a payload large enough to be split before sending;
    a payload the receiver refuses as too large, which is split and resent; a retried send; a
    change of leader; a restart with the plan rows supplied in a different order (and, once the
    tenant config exists, with the tenants listed in a different order in the config file); and a
    removed tenant whose target name is then used by another tenant.
*   The self-telemetry endpoint receives no product telemetry, and no tenant destination receives
    a request carrying the self-telemetry identity.
*   Seeded faults: the same check runs against pipelines that are deliberately mis-assembled
    through a test-only path (two pipelines' emitters swapped, two API keys swapped, a source
    that returns another scope's marker). The test asserts that the check reports each one. A
    check that cannot fail proves nothing.

The tenant shape does not ship while this test fails.

#### 5.4 Run-time evidence

A test exercises this code against a fake. It cannot show what the live vendor returned.

*   The plan hash and row count are logged each time a replica becomes leader.
*   `emitted_total` carries `tenant` and `pipeline`.
*   Every resource emitted in the tenant shape carries the pipeline id (section 6).
*   `scope_assertion_total` counts outcomes by `tenant`, `pipeline` and `result`, and
    `scope_state` shows each pipeline's current state.
*   The operator documentation gives the query a stack's owner can run to look for a pipeline id
    that is not theirs.

### 6. Identity on the wire

Each pipeline's emitter stamps the identity from its row. The emitter already takes its identity
as a map of attributes when it is built. In the tenant shape the assembly adds one more entry to
that map: the resource attribute `genai_otel_bridge.pipeline`, whose value is the pipeline id.
The name is a constant, like `service.name` today.

*   The value is a pure function of static config, so a replay by a new leader encodes the same
    bytes.
*   On Grafana Cloud a resource attribute that the stack does not turn into a label appears only
    on the `target_info` series for that identity. Because every pipeline in a stack has its own
    identity, there is one `target_info` series per pipeline, and joins on it keep working. The
    query in section 5.4 is a query on `target_info`.
*   If a stack does turn the attribute into a label, renaming a tenant or a target changes that
    label, so every series that pipeline emits there becomes a new series.
*   What it adds beyond the identity: two tenants in different stacks may choose the same
    identity values, but a pipeline id is unique in the install. Misdelivered data can be told
    from the stack's own data even when the identities are equal.
*   It is evidence, not routing. Telling two pipelines apart inside one stack relies on the
    identity, which Grafana Cloud's OTLP endpoint already turns into labels.
*   The legacy shape adds nothing and emits today's three attributes.

### 7. Checkpoints

**Key format.** A checkpoint key has three string fields, joined with `/`: the source instance,
the loop name and an output fingerprint. The fingerprint is a hash the source package computes
from the set of series a loop emits and their naming config. Two loops in one pipeline differ in
the loop name. The ConfigMap store turns the joined string into a data key by replacing
disallowed characters, cutting it at 200 characters and appending a 12-character hash of the full
string.

*   Legacy shape: the source instance is the configured `source_instance`, untouched.
*   Tenant shape: the source instance is `mt.<tenant>.<target>`. Tenant ids and target names
    cannot contain `.`, so two different pairs cannot produce the same string.
*   The loop name and the fingerprint are produced as today.
*   The destination and the scope are not in the key. A target pointed at a different destination
    or given a different scope keeps its watermark. The new destination or scope receives data
    from the watermark onward, and nothing earlier is sent again. To start a fresh history, use a
    new target name.
*   Nothing parses a key back into its parts.
*   A legacy `source_instance` may already have the form `mt.a.b`. If that install is later
    converted and has a tenant `a` with a target `b`, the two share checkpoints. The binary logs a
    warning at start when a legacy `source_instance` begins with `mt.`.

**Record format, store and Kubernetes client.** Unchanged. One ConfigMap, the existing one, by its
existing name. Question Q5 decides whether the lease needs its own client.

**Orphaned keys.** The store has no delete and refuses writes above 900 KiB, which is roughly
3,600 keys. A renamed tenant or target leaves its old keys behind. A tenant id that is removed and
later added again picks up its old watermark, and the time since that is older than the lookback
is counted as skipped. An offline prune is future work.

### 8. Self-telemetry

*   The telemetry factory returns hooks that add the labels `tenant` and `pipeline` to every
    self-metric they record. This covers the scheduler's and runner's metrics, the source hooks,
    the upstream latency histogram and the two emit-request metrics. The guard's
    `new_label_values_total` gets the same two labels, because in the tenant shape a guard belongs
    to one pipeline.
*   The metrics interface the scheduler and runner use does not change. The existing `loop` label
    keeps its value. Metric and label names are published contracts.
*   In the legacy shape the factory returns hooks that add nothing, so every label set is
    identical to today's. A legacy config with two sources that share a loop name still
    overwrites its lag gauge, as today. The fix for such an install is to move to the tenant
    shape.
*   The upstream latency histogram already has a label called `target`, a published name that
    means the host of the request. It is unrelated to a target in this document and is not
    renamed.
*   Log lines from the runner, the scheduler and the emitter's partial-reject warning carry
    `tenant` and `pipeline`. The keys that rate-limit repeated log lines include the pipeline id,
    so one tenant's failure cannot suppress another's warning.
*   New signals. Both gauges are declared with no unit, so their names in the metrics backend
    are `genai_otel_bridge_scope_state` and `genai_otel_bridge_tenant_blocked` with no suffix.
    *   `scope_state`: a gauge per pipeline with a `state` label of `matched`, `unverified` or
        `mismatched`. The current state is 1 and the others 0. With no result, all are 0, which
        is what a replica that is not leading reports. Rules take the maximum across replicas.
    *   `scope_assertion_total`: a counter whose `result` is `matched`, `mismatch`,
        `unverifiable` or `error`. It moves each time the gate calls the assertion: after every
        collect that produces a batch, and at least every ten minutes for a pipeline that is
        unverified or mismatched.
    *   `tenant_blocked`: a gauge with the labels `tenant` and `reason` and no `pipeline`. It is
        1 for a blocked tenant and 0 for every other tenant.
*   Liveness stays one heartbeat for the install. Its threshold is derived from the plan.

**Alerts and dashboards.** The behaviours below are the acceptance criteria. The rule expressions
are written and tested in the change that edits the rule files.

*   Every existing rule that groups by `loop` also groups by `tenant` and `pipeline`. That
    changes nothing for series without those labels, so a legacy install's rules behave as today.
    This covers the alerts for a stale poller, failing emits, auth errors, data loss, queue
    backpressure, buckets revised after settle and a cardinality spike, and the five recording
    rules built on the time of the last success.
*   The stale-poller alert and the two baseline rules judge only pipelines whose state is
    matched, and their baselines leave out time a pipeline spent in another state. Series with no
    `tenant` label are judged as today.
*   The leader-absent alert fires when no loop has succeeded for a time. An install whose tenants
    are all idle after a restart has no successes. The alert therefore also treats any increase
    in `scope_assertion_total`, whatever its result, as a sign that a leader is running.
*   New alerts: scope mismatched for 5 minutes (pages); tenant blocked for 5 minutes (pages);
    scope assertion returning only errors for 30 minutes (warning), which covers a pipeline
    stuck with no result; and unverified for 24 hours (low severity).
*   The self-telemetry dashboard gains tenant and pipeline filters and a scope state panel.

### 9. Failure handling

Every signal in a "Run" row carries `tenant` and `pipeline`.

| Failure | Phase | Behaviour | Signal or outcome |
| --- | --- | --- | --- |
| A broken load-phase or resolve-phase rule | Load, resolve | The process does not start. The config check finds it in CI. | Exit. The new pod never becomes ready, so the rollout stops and the old pods keep running. |
| A broken assemble-phase rule | Assemble | The process does not start. | The same. The config check does not find it. |
| A tenant's secret file is missing or unreadable | Assemble | That tenant's pipelines are not built. Every other tenant starts. If every tenant is blocked, the process does not start. | `tenant_blocked` with reason `secret_unreadable` |
| 401 or 403 from the vendor, on a collect or on the scope assertion | Run | That loop does not advance and retries at its cadence, as today. | `auth_errors_total` |
| The scope assertion call fails for another reason | Run | Any collected batch is dropped. The loop retries at its cadence. | `emit_errors_total` with kind `collect`; `scope_assertion_total` with result `error` |
| Scope mismatch | Run | Batch dropped. Each loop is retried every ten minutes until a `Matched` result. | `scope_state`; `loop_degraded` with reason `scope_mismatch` |
| Scope unverifiable | Run | Batch dropped, no watermark moves. Asserted again on each tick, at most once a minute. | `scope_state` |
| Destination rejects or is slow | Run | That pipeline's loops retry or degrade, as today. | `emit_errors_total`, `loop_degraded`, `emit_partial_success_rejected_total` |

A blocked tenant has no loops, so it has no lag series. The `tenant_blocked` alert must page
inside the lookback, because past it that tenant's metrics have a permanent, counted gap.

Fixing a tenant's API key or token, or supplying a missing secret, means changing its Secret and
restarting the pods. A Secret change does not restart them by itself, because the chart only
restarts pods when the config changes. The documentation gives the restart command.

### 10. Legacy compatibility

The legacy shape is not a second code path for assembly. It resolves to plan rows with no tenant
id, and the same three passes run. What keeps it identical:

*   `source_instance` is passed through, so checkpoint keys do not change.
*   One guard and one series-ownership group, because every legacy row shares one stack and one
    identity.
*   No pipeline attribute, no `tenant` or `pipeline` labels and no scope gate. `emit.self` still
    falls back to `emit.telemetry`. Secrets are read at load and an unreadable one is fatal, as
    today.

Golden tests land before any refactor and pin, as literal expected values: the ConfigMap data key,
the DynamoDB key and the file key for a known checkpoint key; the encoded OTLP bytes of a known
batch; and the label sets of every self-metric. An upgrade test starts the new binary on a
ConfigMap written by the current version.

Converting a running legacy install to the tenant shape is a migration, not an upgrade:

*   Its checkpoint keys change, so every loop starts from its first-run lookback and sends the
    last settled buckets again. With an unchanged identity the backend treats them as repeats.
    With a new identity they are new series.
*   An idle workspace changes from "emits zeros" to "emits nothing".
*   It is two steps: the new image first, with the legacy shape, then the tenant-shape config. An
    old-image pod that restarts onto a tenant-shape config would exit on the config keys it does
    not know. `helm rollback` to a revision before the tenant shape stops every tenant's
    pipelines.

### 11. Chart and deployment

*   Tenants are set under the chart's existing `config` value, as `config.tenants` and
    `config.defaults`. The chart's `config` value carries active defaults for the legacy shape.
    When `config.tenants` is set, the chart renders the config without `sources` and
    `emit.telemetry`. The chart also accepts a whole config as one string (`configOverride`). The
    chart reads the tenant ids from whichever of the two it renders.
*   Each tenant's secrets are read from a Secret named `genai-otel-bridge-tenant-<tenant id>`. The
    chart mounts one per tenant, read-only and optional, at
    `/var/run/secrets/genai-otel-bridge/tenants/<tenant id>/`. Optional means a missing Secret
    does not stop the pod from starting. That path is outside the read-only config mount.
*   The install's own Secret, which holds the self-telemetry credentials, is still required.
*   When `config.tenants` is set, the chart fails to render unless the rolling update's
    `maxUnavailable` is 0. Then a new pod must become ready before an old one is removed, and a
    pod that fails to load or assemble its config never becomes ready. The chart's default of 1
    would let the only old pod of a single-replica install be removed first.
*   The NetworkPolicy is not changed. Its single OTLP egress range defaults to everything. An
    install that has narrowed it must cover every tenant's destination.
*   CI runs the config check with the new image before a config change is merged.

A rollout pauses every pipeline for one or two leadership handovers. One handover takes about 20
to 30 seconds: the lease duration (15 seconds), the retry period (2 seconds) and a first collect.
Every pipeline then resumes from its checkpoints. A rollout also discards every pipeline's
assertion result, so each asserts again before its first collect, and clears the in-memory record
the `analytics` loop keeps to detect buckets revised after settle.

### 12. What shards will need

Not specified here. Listed so the decisions above can be checked against them.

*   Lease and checkpoint names as settings, with chart names and access rules derived from the
    release.
*   A stamp on each checkpoint record naming the lease that wrote it, with a foreign stamp refused.
    The leader number in a watermark is one lease's count of leadership changes, and counts from
    two leases cannot be ordered. The stamp can be added later: a record without one is adopted
    on first write. A lease recreated under the same name can be adopted automatically when a
    replica becomes leader. A move between leases needs an explicit, scoped command.
*   One tenant registry with a sticky shard field, from which every shard's values are rendered.
*   A shard label on self-telemetry and per-shard alerts.

The checkpoint key contains no shard name, so a later shard design does not rewrite keys.

## Risks and Mitigations

| Risk | Mitigation | What remains |
| --- | --- | --- |
| An API key with wider access than its scope delivers another team's data. | Teams issue a key bound to one workspace and limited to reading analytics. The scope assertion after every collect. The pipeline attribute as evidence. | The assertion sees traffic, not permissions, and for a wider key it rests on a vendor behaviour that is observed, not documented (Q6). Q6 is run again for each version that changes the Portkey package. Nothing detects the behaviour changing between runs. |
| A bug pairs a loop with the wrong emitter. | One place of assembly; the release gate with seeded faults. | A test proves the code against a fake, not against production. |
| One process holds every team's source API key and destination token. | Secrets are files in one directory per tenant and are never placed in the config tree, so a config error cannot contain one. Each emitter already removes its own credentials from error text. Teams should issue a destination token limited to writing metrics. | Anyone who can read Secrets in the install's namespace, or exec into its pods, can read every tenant's credentials. Access to that namespace is the central team's alone. The author accepted this. |
| A restart or a bad version pauses every tenant in the install. | Recovery is from checkpoints with no gap inside the lookback; `maxUnavailable: 0`; a ceiling of about 25 tenants per install. | A pause of up to a minute per rollout. A tenant that needs more isolation gets its own install (Alternative B). |
| The vendor's request budget is shared across keys. | Q1 is answered before change 2. Cadence is the first control: one analytics request returns up to 55 one-minute buckets, so a five-minute cadence makes about a fifth of the requests with no loss of data. | If the shared budget supports fewer than 20 tenants the design is stopped. |
| The vendor's own OpenTelemetry export makes polling unnecessary. | Q2 is answered before change 2. | None after Q2. |
| Every checkpoint save rewrites one ConfigMap. | Q5 measures it before change 6. Alternative L is the first response. | The store holds every key ever written until an offline prune exists. |
| The change breaks an existing single-team install. | Golden tests and an upgrade test land first and stay green through every change. Changes 2 to 5 do not alter a legacy install's emitted telemetry, checkpoint keys or self-metric label sets. | A legacy install with two sources sharing a loop name keeps today's overwritten lag gauge. |
| An idle or newly added tenant looks broken because nothing is emitted. | An explicit unverified state, left out of outage alerts, with its own low-severity alert; stated in the tenant data contract. | A stack shows no data, not zeros, for a workspace idle for seven days. |
| A rule that needs a built source is broken, and the config check does not find it. | The pod fails to become ready and the rollout stops with the old pods running. | The mistake is found at rollout, not in CI. The config is one ConfigMap updated in place, so an old pod that restarts during a stopped rollout reads the new config and fails too. A stopped rollout must be rolled back promptly. |

## Alternatives Considered

### Alternative A: Add a destination and identity to each existing source entry

*   **Pros**: The smallest change to the config. The `sources` list already gives several API
    keys, rate limiters and checkpoint namespaces in one process.
*   **Cons**: `sources` is a list, and Helm replaces a list when values files are layered, so a
    registry of tenants cannot be kept as one values file per tenant. A source entry has no owner:
    there is nothing to hang "two tenants may not share a stack" on, no id for the self-metric
    label, and no directory to mount a team's secrets under. `source_instance` has no character
    rules beyond "no slash", so it cannot safely become a label and a path.
*   **Reason for Rejection**: Rejected by author: the tenant is the unit the operator adds,
    removes and is paged about, and the config should name it.

### Alternative B: One install per tenant, with or without a Kubernetes operator

*   **Pros**: No change to the gap-free mechanisms or to any frozen interface. Each tenant keeps
    its own lease and checkpoints. A failure or restart affects one tenant. Adding a tenant
    restarts nobody else. Each pod holds one team's secrets. It needs no Kubernetes operator: a
    generated values list with one namespace per install works with today's chart.
*   **Cons**: One pod per tenant with a single replica, two with a standby. About ten Kubernetes
    objects per tenant. One rollout per tenant for every upgrade.
*   **Reason for Rejection**: Rejected by author: the pod guide allows 0.4 pods per tenant, and
    this shape uses one or two at every tenant count. It stays supported as a placement for a
    tenant that needs hard isolation, and it is the fallback if the release gate cannot be made
    to report every seeded fault.

### Alternative C: Launch at one tenant per install, build native multi-tenancy behind it

*   **Pros**: The first tenants get data with no change to existing code paths. The riskiest work
    is hardened before any tenant depends on it.
*   **Cons**: Over the pod guide during the launch period. Every launch tenant must later be moved
    from its own lease into a shared one, and the checkpoint fence does not hold between two
    leases.
*   **Reason for Rejection**: Rejected by author: native multi-tenancy is needed by the time there
    are 50 tenants in any case, so the move would be made for every launch tenant.

### Alternative D: Sharding inside the binary with fixed slot leases

A fixed number of slots, each with its own lease; each tenant is assigned to a slot by a hash of
its id; pods compete for slot leases.

*   **Pros**: One install and one config, so no drift between shards and no tenant in two places.
    Standby capacity is pooled. It alters no frozen interface: several coordinators can each drive
    a scheduler.
*   **Cons**: One rollout and one config load for every tenant. No way to try a version on one
    shard first. Without a rule for which pod takes which slot the first pod takes them all, and
    that rule decides which replica emits. Every pod holds every secret. The slot count and the
    hash become permanent. The workload has no elasticity to use: load is a fixed function of
    pipeline count and cadence.
*   **Reason for Rejection**: Rejected by author: it puts a balancing protocol where the choice of
    emitting replica is made, for scaling the workload does not need. The scheduler, coordinator
    and store stay usable more than once per process, so the option is not closed.

### Alternative E: Hash-mod shards of one Deployment

*   **Pros**: A familiar pattern. No registry of placements.
*   **Cons**: Changing the shard count moves pipelines between leases. Leader numbers from
    different leases cannot be ordered, so every resize has a window with no checkpoint fence.
*   **Reason for Rejection**: Rejected by author: it breaks the checkpoint fence on every resize.

### Alternative F: One shared watermark for a scope sent to several destinations

*   **Pros**: One set of vendor requests feeds every destination.
*   **Cons**: One destination's failure stalls the others. Holding batches for the slow one is a
    write-ahead log, which the architecture's decision ledger rejects (row #8: an in-memory queue
    plus replay from the vendor, no on-disk log).
*   **Reason for Rejection**: Rejected by author: it couples destinations.

### Alternative G: An OpenTelemetry Collector fan-out for a second destination

*   **Pros**: No doubled vendor requests. A tenant runs it behind its own endpoint.
*   **Cons**: Rejections by the final backend become invisible to the bridge's counters.
    Byte-identical encoding on the last hop moves out of the binary. As a way to route between
    tenants it moves the cross-tenant risk into collector config.
*   **Reason for Rejection**: Rejected by author as the routing mechanism between tenants. A tenant
    may still put a collector behind its own destination.

### Alternative H: One organisation-level API key with all workspaces aggregated

*   **Pros**: One credential for a whole organisation.
*   **Cons**: It returns one aggregate, not a series per workspace that can go to different stacks.
*   **Reason for Rejection**: Rejected by author: it cannot route per workspace.

### Alternative I: One process per API key, several containers in a pod

*   **Pros**: Unrelated API keys never share a process. No per-tenant guard or self-metric work.
*   **Cons**: Each container runs its own leader election, so there is one lease per tenant, with
    the write load of Alternative J. Each container pays the Go runtime's baseline memory and
    needs its own config file and its own probes. Adding a tenant changes the pod template, which
    restarts every container in the pod.
*   **Reason for Rejection**: Rejected by author: it keeps the lease-per-tenant cost and the
    all-tenant restart while adding per-container plumbing.

### Alternative J: A lease per tenant inside one process

*   **Pros**: The tenant becomes the unit of the checkpoint fence, so a tenant could move freely.
*   **Cons**: About 125 lease writes a second at 250 tenants on the default timings. Every pod
    holds every secret.
*   **Reason for Rejection**: Rejected by author: the write load buys a freedom the design does not
    need.

### Alternative K: Write-behind checkpoint saves

*   **Pros**: Far fewer writes to the API server.
*   **Cons**: A watermark is acknowledged before it is durable.
*   **Reason for Rejection**: Rejected by author: it weakens the third gap-free mechanism.

### Alternative L: Saving waiting checkpoints together

*   **Pros**: The store already reads and rewrites the whole ConfigMap for each save. Applying
    every waiting key in one read-modify-write, and releasing each caller only after that write is
    durable, cuts API calls without acknowledging anything early.
*   **Cons**: One failed update or conflict retry affects every pipeline whose save is in that
    write. It adds concurrency on the path of the third gap-free mechanism.
*   **Reason for Rejection**: Deferred, not rejected: no need is measured. It is the first response
    if Q5 fails on save rate.

### Alternative M: Re-reading secret files while running

*   **Pros**: A rotated API key, a late Secret or a fixed key heals one tenant with no rollout and
    no pause for the others.
*   **Cons**: The running credential is no longer fixed when a replica becomes leader, so a scope
    result must be tied to a credential version. A per-tenant fix no longer passes through the
    config check. More code on the credential path.
*   **Reason for Rejection**: Rejected by author for the launch: at 10 to 20 tenants a restart is
    rare and its cost is one or two handovers. It is the first thing to add as tenant count grows.

### Alternative N: The vendor's own OpenTelemetry export

*   **Pros**: Push from the vendor would need no polling, no watermark and no rate budget.
*   **Cons**: Not assessed. Push has no replay, so loss while the receiver is down is permanent.
*   **Reason for Rejection**: Open, not rejected. It is question Q2, which must be answered before
    change 2.

### Alternative O: Assert the scope on a timer, before the collect only

*   **Pros**: One assertion every few minutes per pipeline in place of one per collect.
*   **Cons**: A bucket can close just before an assertion and be ingested by the vendor just after
    it. The bucket is then emitted on a result that predates the traffic. The timer would have to
    be tied to each loop's settle, which for one Portkey loop is a setting local to the vendor
    package that core cannot read.
*   **Reason for Rejection**: Rejected by author: its safety has no margin and depends on a
    vendor package's settings.

### Alternative P: Put the scope policy inside each loop, or in a wrapper around it

*   **Pros**: Portkey's loops already assert at the top of a collect, and the scheduler already
    maps two kinds of loop error to "discard and do not advance" and to the degraded state. A
    second assertion at the end of a collect, with two new error values, needs no gate object and
    no change to the scheduler's per-loop record.
*   **Cons**: The policy would be written once per loop of every vendor package, in the code most
    likely to differ between vendors. It could not skip the lag accounting, which the scheduler
    does before a collect, so an idle tenant would look stale. A wrapper around a loop would hide
    the optional capabilities the composition root detects on a loop by type.
*   **Reason for Rejection**: Rejected by author: the policy is vendor-neutral and belongs in one
    place, and it must be asked before the scheduler's accounting.


## Non-Functional Requirements

**Security and privacy.**

*   Credentials: see the Risks section.
*   Content minimisation is unchanged. Every guard denies the same content fields. Some of them
    can never be released. A per-loop opt-in can release one of the others, for that pipeline
    only.
*   Self-telemetry names every tenant and pipeline, and the plan table names every scope id. Both
    go only to the central team: self-telemetry to `emit.self`, the plan table to CI output.
*   The config check prints scope ids, URLs and instance ids. None is a secret.

**Performance.**

*   Expected steady state at 20 tenants with two loops each at a 60-second cadence: under one
    checkpoint save a second, and about three vendor requests a second. After a restart each
    tenant makes about 10 requests, paced by its own rate limiter. Q1, Q3 and Q5 test whether one
    install sustains this.
*   Each pipeline's guard holds its own seen-values set, up to the cardinality budget per metric
    name. Q3 measures the footprint at the full budget.

**Rollout and rollback.**

*   The work lands as a sequence of changes that do nothing in the legacy shape, then one version
    that accepts the tenant shape. From that version the items in G8 are fixed.
*   Each change before that version can be rolled back by image.
*   A tenant team is told, in a one-page contract: data arrives about ten minutes behind because
    of settle; a gap longer than the lookback is permanent and counted; a rollout of the shared
    install pauses delivery for up to a minute; nothing is emitted before the workspace's first
    traffic, or after seven days without traffic.


### Additional Resources

*   `ARCHITECTURE.md`: the frozen interfaces and the decision ledger. This design amends these
    rows:
    *   #1 (generic and decoupled; a single-tenant tool was rejected): one install may now serve
        many tenants.
    *   #6 (one leader through a lease; sharding was rejected as needless): still one leader per
        install. Shards as extra installs are recorded as the planned next step.
    *   #7 (checkpoints in one ConfigMap): the tenant-shape key format.
    *   #9 and #18 (self-telemetry signals and their closed label sets): the two new labels and
        the three new signals.
    *   #13 and #16 (both rely on the series-ownership check being process-wide): the check is now
        per stack and identity.
    *   #17 (the ECS and DynamoDB deployment): it does not support the tenant shape.
*   `docs/DESIGN.md`: the statement that the intended production pattern is one process per
    environment or workspace is amended.
*   `docs/high-availability.md`, `internal/source/portkey/AGENTS.md`, `followup.md`.
*   Portkey API key types:
    https://docs.portkey.ai/docs/product/enterprise-offering/org-management/api-keys-authn-and-authz
*   Portkey analytics parameters:
    https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/graphs-time-series-data/get-requests-data

### Future Improvements

*   **Shards**: section 12.
*   **Re-reading a tenant's secret file on an auth failure** (Alternative M).
*   **A way to tell which scope an API key belongs to that does not need traffic.** It would let
    an idle tenant's zero-valued series be emitted.
*   **A standing check of the vendor behaviour in Q6**: a target on a deliberately wide key that
    must report mismatched, with an alert if it ever reports matched.
*   **API keys that cover several scopes**: one target per scope under a wider key, after a live
    investigation shows every endpoint used honours a scope parameter, including with a bad or
    missing value.
*   **Scope discovery as a detector**: list scopes, compare with the plan, and signal the ones
    with no target. An operator adds the target.
*   **Log records in the tenant shape**, after Q7.
*   **Per-API-key use-case labels on a target.**
*   **Per-tenant governance settings.**
*   **LangSmith in the tenant shape**, which needs a scope assertion for that source.
*   **Retiring the legacy `expected_workspace` check**, by giving the legacy shape an optional
    scope id that uses the gate.
*   **An offline prune of orphaned checkpoint keys.**
*   **A Kubernetes operator and a tenant resource**, only if an outside system must add tenants
    without a reviewed config change.

## Order of work

The work lands in six changes, in this order. Some changes are more than one pull request. Each
row below is one issue and one pull request. The step-by-step implementation plan is not part of
this document: it is written into the issues.

| Unit | What it is | Implements | Needs first |
| --- | --- | --- | --- |
| 1 | Compatibility harness, tests only | Section 10, goal G6 | Nothing. Can start at once. |
| 2 | Tenant and pipeline labels in self-telemetry | The labels and log attributes of section 8, goal G4 | Q1 and Q2 pass |
| 3a | The plan package and the config check's output | Section 3 | Unit 2 |
| 3b | The three passes of assembly and the two factories | Sections 4, 5.2 and 6 | Unit 3a |
| 4 | The scope gate | Section 5.1 and its signals in section 8 | Unit 3b; Q6 passes |
| 5a | The race detector on the acceptance recipe | Verification | Unit 3b |
| 5b | The release gate and the isolation tests | Section 5.3, goals G2 and G5, the run-time cases of G3 | Units 4 and 5a |
| 6a | The tenant config schema and its load rules | Section 2 (load phase), goal G9 | Unit 3b |
| 6b | Resolving, secrets and assembly for tenant rows | Sections 2 (resolve and assemble phases), 7 and 9, goal G8 | Units 5b and 6a; Q3 and Q5 pass |
| 6c | The chart | Section 11 | Unit 6a |
| 6d | Alert rules and the dashboard | The alerts of section 8 | Units 4 and 6b |
| 6e | Switching the tenant shape on | Goals G1 and G7, the unreadable-secret case of G3, the evidence query of section 5.4, the documentation and the design record | Units 6b, 6c and 6d |

From unit 6a until unit 6e, the binary refuses to start with a tenant-shape config and says the
shape is not enabled in this version, and the config check says the same. Before unit 6a a
`tenants` key is an unknown key, which the loader already refuses. Units 6a to 6d are exercised
by tests. Unit 6e removes the refusal. It is the only unit whose commit type is `feat`, so the
changelog does not announce multi-tenancy before it works, and the formats goal G8 fixes are fixed
by that unit's version.

Units 2 to 5b change nothing in the legacy shape's emitted telemetry, checkpoint keys or
self-metric label sets. Two things a legacy install can see do change: from unit 3a,
`-validate-config` also prints the plan table and hash; from unit 3b, a replica logs the plan
hash and row count each time it becomes leader.

Related hardening found during this design is not part of it. Each item is filed as its own issue:
a checkpoint save refused because of an older leader number that never backs off; a leader that
starts its scheduler after failing to read its leader number; quota and auth failures that retry at
full cadence; and three stale statements, in `ARCHITECTURE.md`, `docs/high-availability.md` and the
`-cleanup-retain-checkpoint` flag help text.
