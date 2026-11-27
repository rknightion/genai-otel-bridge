# Multi-tenancy for genai-otel-bridge

*   **Status:** Approved for implementation. Every question is settled (see "Questions and
    answers"). The units are issues #520 to #536.
*   **Date:** 2026-11-26. Revised for the rule that one API key serves exactly one workspace.

## Overview

genai-otel-bridge polls AI-platform APIs and emits operational telemetry as metrics and logs over
OTLP, the OpenTelemetry protocol. Today one process serves one team: it has one OTLP destination,
one identity, one governance budget and one set of checkpoints. A team that wants telemetry from
its LLM gateway gets its own install.

This design lets one install serve many teams. A central observability team runs it. Each team hands
over an API key for its workspace in Portkey, an LLM gateway, and names that workspace. That
workspace's telemetry is sent to that team's own OTLP destination under that team's own credentials.
One API key serves exactly one workspace. The launch supports Portkey workspace keys. A key that
can read several workspaces (a Portkey organisation key) will be accepted for the one workspace
its configuration names once question Q6 is run; the design already sends that workspace on every
request. Portkey is
the first source. The expected size is 10 to 20 teams at launch and about 250 later.

This document specifies the launch: one leader-elected install that runs many isolated pipelines. It
does not specify shards (a second install with a different list of tenants). It checks only that
nothing decided here has to be undone for them. One install has a ceiling, because a restart pauses
every tenant in it and because every checkpoint save goes through one object. The working ceiling is
set at about 25 tenants. That figure is an estimate, taking one pipeline with two loops per tenant,
and the author accepted it without measurement (Q3, Q5). The size of 250 is a forecast and is not
tested here.

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
*   **Target.** One source API key plus one scope. An API key serves one target only.
*   **Scope.** The one thing a target's API key reads, given by two strings: a name and an id.
    Core treats both as opaque and does not filter by them. For Portkey the name is the workspace
    slug and the id is the workspace id: the analytics API takes the slug and the log export takes
    the id. The Portkey package sends the slug on every analytics request. A workspace key ignores
    it, because Portkey binds that key to one workspace. An organisation key reads the workspace
    the slug names. The scope name is what the scope assertion compares against.
*    **Key fingerprint.** The SHA-256 of an API key value, taken from the exact header value the
    source sends, after the loader trims the secret file. A trailing newline in one file and not
    another therefore gives the same fingerprint. It is computed in memory when the key is read,
    compared there, and written only inside a remembered scope match (section 7). It is never
    logged, emitted or shown in the plan table.
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
    Startup fails when two loops declare the same metric name. Self-metrics carry the source
    instance and the loop name, but nothing names the team, so alerts cannot be routed per team.
    The guard's budget is shared.
*   **Putting many teams in one process creates a new failure: one team's data delivered to
    another team's stack.** Nothing in the binary guards against it, because until now it could not
    happen. The most likely cause is not mis-wiring inside the binary. It is an API key that reads
    something other than what the config says: the wrong key pasted for a tenant, one key
    configured for two tenants, or an organisation key whose workspace selector is wrong or not
    honoured.

The solution is to make the pipeline the unit of isolation. Each pipeline is assembled in one place
from one row of a resolved plan, and carries its own guard, emitter, self-metric labels and
checkpoint keys. A key may serve only one pipeline. Before it collects, a pipeline checks that its
API key reads its configured workspace, and it repeats the check on a timer. The existing
single-team config keeps working unchanged.

## Goals

Each goal states how it is checked.

*   **G1. One install serves many tenants, each to its own destination with its own credentials.**
    Test: the k3d end-to-end test runs three tenants and each fake stack receives only its own
    tenant's records.
*    **G2. Five controls guard against one tenant's data reaching another tenant's destination.**
    They are: a scope assertion at the source that fails closed, one API key per target, one place
    where a pipeline is assembled, a release gate that is shown to fail on seeded faults, and
    run-time evidence an operator can query. Section 5.1 and the Risks section state what they do
    not catch. Check: the release gate in section 5.3 passes, including its seeded faults.
*   **G3. One pipeline's run-time failure does not stop or delay any other pipeline.** Tests: with
    one destination returning 503 for 10 minutes, every other pipeline's `window_lag` stays within
    settle plus two cadences. A pipeline with a revoked API key or a scope mismatch does not
    advance, a tenant with an unreadable secret is not built, two tenants given the same API key
    are both not built, and in each case every other pipeline runs.
*   **G4. Every pipeline is individually alertable.** Lag, skips and degraded state carry the
    pipeline's id. Test: two pipelines with the same loop name produce two distinct `window_lag`
    series, each carrying its own `tenant` and `pipeline` labels.
*   **G5. The three gap-free mechanisms work unchanged inside each pipeline.** Test: the existing
    acceptance tests pass with their assertions unmodified, and the same scenarios pass when run
    with three pipelines.
*   **G6. An existing single-team install upgrades with no config change.** Test: golden tests pin
    the checkpoint keys for the three checkpoint backends (ConfigMap, DynamoDB and file), the
    emitted OTLP bytes and the self-metric label sets of the legacy shape. An upgrade test starts
    the new binary on a checkpoint store written by the current version.
*   **G7. The launch fits the pod guide.** Check: the launch install runs two pods, a leader and a
    hot standby (Q8).
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
*   **One API key serving several targets.** A key serves exactly one target, and so one scope.
    An organisation key, once supported (Q6), is accepted for the one scope its target names.
    Pointing it at a second scope is a second key.
*   **Organisation keys at launch.** The launch supports workspace keys only. Nothing in the
    binary tells the two kinds apart, so the operator documentation states it.
*   **Log records in the tenant shape.** A loop that emits log records is rejected until question
    Q7 shows an API key cannot export another scope's logs.
*   **Per-API-key use-case labels in the tenant shape.** The legacy `api_key_use_cases` setting is
    not offered on a target in this version.
*   **Discovery of scopes.** No pipeline is created from a listing.
*   **Fan-out from one collection to several destinations.** A second destination for the same
    scope is a second target in the same tenant, with a second API key for that workspace. It
    makes its own requests and keeps its own watermark. Reusing the first key is a duplicate and
    blocks the tenant (section 5.5).
*   **Two tenants reading one workspace.** A scope belongs to one tenant (section 2).
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

## Questions and answers

Every question the design depended on was settled on 2026-11-26, by research, a live probe or the
author's decision. None gates a unit.

| Id | Question | Answer |
| --- | --- | --- |
| Q1 | What is Portkey's request budget for the analytics API, and is it per key, per organisation or per client IP? | Not documented: Portkey's API reference gives no figure, no scope and no rate-limit headers for the analytics endpoints, and documents 429 only for the rate-limit policies a customer sets on gateway traffic. Accepted by the author without a probe. The launch uses the design's defaults (7 requests per tenant per minute at a 60-second cadence, a per-target rate limiter) and the scheduler's existing quota backoff. Quota errors in production size the install; a five-minute cadence is the first response, because one analytics request returns up to 55 one-minute buckets. |
| Q2 | Would Portkey's own OpenTelemetry export replace polling? | No; polling stays. Portkey documents its OTLP push for Enterprise and self-hosted gateways only, not the cloud API. Its metrics export documents no workspace attribute. Its trace export carries prompt and completion content with no documented way to turn it off. Failed exports are not retried and are lost while the receiver is down. |
| Q3 | What are the memory and CPU of a leader and a standby with 1, 20 and 50 pipelines? | Not measured. The author accepted the design's estimates for a launch of 10 to 20 tenants. Heap use is watched in production. |
| Q4 | On each destination's stack, do the identity attributes become labels, and is the pipeline attribute kept and searchable? | Yes, by Grafana Cloud's documentation: `service.namespace` and `service.name` form the `job` label, `deployment.environment.name` is promoted by default, and every other resource attribute, the pipeline attribute included, is kept on `target_info`. A stack can change its promoted set, so each tenant's onboarding still checks its stack. |
| Q5 | What checkpoint save rate does one install sustain, and are lease renewals delayed? | Not measured. The author accepted the estimate of under one save a second at 20 tenants. Alternative L is the first response if saves become the limit. |
| Q6 | Does every analytics endpoint honour `workspace_slug` for an organisation key? | Deferred with organisation keys. The launch supports workspace keys only (Alternative Q). The slug is still sent on every request, so organisation keys need no code change once Q6 is run with an organisation key over two workspaces that both have traffic. |
| Q7 | Can a workspace API key export another workspace's logs? | Open. Log records stay out of the tenant shape until it is answered. |
| Q8 | Does the launch run a hot standby? | Yes, by the author: two replicas, a leader and a hot standby. |
| Q9 | What does a workspace key return when sent a slug for another workspace? Is a slug unique across organisations? Can a workspace have several keys? | A live probe on 2026-11-26 showed a workspace key ignores the slug: the graph, model-group and workspace-grouping endpoints returned the same rows for no slug, the right slug, a slug naming no workspace, `include_all_workspaces` and `exclude_workspace_slugs`. The workspace grouping returns a slug and no id. Portkey does not document whether slugs are unique across organisations, and its documented slugs carry a random suffix; the residual case is accepted in Risks. Several keys per workspace is implied by Portkey's documentation (a service key can create others) but not stated. |

## Critical User Journeys (CUJs)

### CUJ 1: Add a tenant

*   **Given**: a running install with several tenants, and a new team's API key, its workspace's
    slug and id, and OTLP destination credentials. The key and the token are stored as a
    Kubernetes Secret.
*   **When**: the operator adds the tenant to the chart values, runs the config check, and rolls
    the install.
*    **Then**: the config check prints the plan table, which shows where every target is sent, so
    the change can be reviewed as a diff. After the rollout the new tenant's pipeline asserts its
    scope. If the workspace has had traffic in the last seven days the assertion passes, and its
    metrics appear in its stack once the first buckets are older than settle, about ten minutes
    later. If it has had none, the pipeline reports "unverified", emits nothing, and starts by
    itself on the next tick after traffic arrives. Once it has matched, the match is remembered for
    that key and workspace, so a later idle spell or restart does not stop it. Every other pipeline
    pauses for one or two leadership handovers and resumes from its checkpoints with no counted
    skips.

### CUJ 2: A target's API key is revoked

*   **Given**: a running target whose API key is then revoked at the vendor.
*   **When**: that pipeline's next request to the vendor returns 401 or 403.
*   **Then**: that pipeline's loops stop advancing. An auth-error signal carrying the tenant and
    pipeline ids fires. No other pipeline's lag changes. When the operator replaces the key in the
    Secret and restarts the pods, the new key has a new fingerprint and is asserted afresh. Once
    it matches, which needs traffic in the workspace in the last seven days, the pipeline resumes
    from its watermark and reads back up to its lookback.

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

### CUJ 7: One API key is configured for two targets

*   **Given**: two targets, in one tenant or in two, whose secret files hold the same API key.
*   **When**: a pod starts and reads the tenants' secrets.
*   **Then**: neither target's tenant is built. `tenant_blocked` with reason `duplicate_key` is 1
    for each, the tenant-blocked alert pages, and a log line names both pipeline ids and never
    the key. Every other tenant runs. The config check cannot find this, because it never reads
    secrets.

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
        +-- scope gate                asked by the scheduler before each collect
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
    pipeline's scope gate. The scheduler asks the gate before each collect.
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
    loops:                            # every target of this source type uses these blocks
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
        scope:                        # required; the one workspace this key reads
          name: example-workspace-slug
          id: example-workspace-id
        destination: main
        # identity: {...}             # omit to use the tenant's identity
        # a target has no loops block: every target uses defaults.<type>.loops
```

The `governance`, `queue`, `ha`, `log` and `selfobs` blocks keep their meaning and apply to the
whole install. There is no per-tenant form of them in this version: the cardinality budget and
the label-key opt-in list are the same for every tenant. Each target's source has its own rate
limiter, set by `source.rate_limit`.

Rules. A load or resolve rule that fails stops the process. An assemble rule that fails stops the
process, except the two that block one tenant: an unreadable secret and a duplicate key (section
9). Section 3 says which phase checks each rule.

*   Tenant ids and the names of destinations and targets match
    `[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?`. A rename is not blocked. A renamed tenant or target
    starts new watermarks and leaves its old checkpoint keys behind, and the documentation says
    so.
*   `tenants`, `destinations` and `targets` are maps, never lists, because Helm replaces a list
    when values files are layered and merges a map.
*   A target names one destination inside its own tenant. There is no syntax for referring to
    another tenant's objects, no pattern and no precedence.
*   `scope` is required for every target, with both `name` and `id`, neither containing a
    control character. A target must not set the
    legacy `settings.expected_workspace`; the scope replaces it.
*   No two targets, in one tenant or in two, may use the same source API key. This is checked
    when the pod reads the secrets (section 5.5), not by the config check.
*   No scope name and no scope id may appear in two tenants. One tenant may name one scope in
    two targets, with two keys, to send it to two destinations. This rule is what stops an
    organisation key from being pointed at another tenant's workspace: the scope assertion cannot
    catch that, because an organisation key reads whatever workspace it is sent (section 5.1).
*   A tenant's `identity` is required, with both fields. It does not fall back to the install's
    top-level `identity`.
*   A target with an `identity` block uses that block, whole, in place of the tenant's.
*   A target has no `loops` block. Every target uses the blocks in `defaults.<type>.loops`. Each
    of those blocks must be complete: after the loader applies the defaults it applies to a legacy
    loop block today (settle), it must pass the validation a legacy loop block passes. The same
    holds for a tenant source and the legacy defaults for its rate limit.
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
the tenant id, the target name, the source settings, the scope, the destination, the identity,
the loop configs and the pipeline id.

*   The **plan table** is the plan rendered as text with no secrets: tenant, target, source type,
    source URL, scope name and id, pipeline id, destination name, endpoint, instance id, identity
    and enabled loops.
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
`source_instance` (the existing per-source key that names its checkpoints) in the pipeline id
column.

Rules are checked in three phases:

| Phase | Where | Checks |
| --- | --- | --- |
| Load | Config package | Shape, ids and names, required keys, complete loop blocks, the https rules, the form of the secret file names, no references under `tenants` or `defaults`, `ha` values. |
| Resolve | Plan package | References between a target and its destination, the two stack-sharing rules, the self-telemetry rule, no scope in two tenants. |
| Assemble | Composition root | Rules that need a built source and its loops, or the secrets: no two targets with the same API key (section 5.5; blocks the tenants, does not stop the process), scope assertion support, no vendor loop named `scope-match`, no loop that emits log records, no two loops in a group declaring one metric name (section 4). |

The config check runs the load and resolve phases, then builds the app with placeholder credentials,
an inert emitter and checkpoint store and no network, as it already does for the legacy shape. So
every assemble rule that needs no secret is found in CI. It never reads a tenant's secret files, so
two rules are found only when a pod starts: a duplicate API key and an unreadable secret. Both block
the tenants concerned, not the process (section 9). The chart requires a new pod to become ready
before an old one is removed (section 11), so a pod that fails to assemble never becomes ready and
the old pods keep running. A replacement pod from an old ReplicaSet mounts the same
[immutable config revision](../installation.md#config-revisions). Secret values can change
independently (see Risks).

### 4. Pipeline assembly

The composition root assembles pipelines from the plan in three passes.

1.   **Build sources.** For each tenant, read its secret files through a reader the caller supplies,
    and compute the key fingerprint of each target's API key. Block every tenant that has a target
    whose fingerprint another target also has (section 5.5). For each row, ask the telemetry factory
    for the row's telemetry hooks, build the source hooks from them, and build the source through
    the existing registry. A source takes its hooks when it is built, so the hooks must exist first.
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
scope name and id. The scope is a new field on that struct which a legacy config file cannot set.
Pass 1 adds the secret values to a copy of the row. The plan table never shows them.

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

The most likely wrong delivery is an API key that reads something other than what the config
says. A check on the binary's wiring cannot catch it, because the wiring is right and the data is
wrong.

A source may implement one optional capability:

```go
// ScopeAsserter is implemented by a Source that can report what its API key reads.
// Matched means: the source's assertion request, sent as its loops send theirs, returned only
// the configured scope, from traffic in the source's assertion window.
type ScopeAsserter interface {
    AssertScope(ctx context.Context) (ScopeResult, error)
}
// ScopeResult is Matched, Mismatch or Unverifiable. An error means the call failed.
```

**What the Portkey package sends.** When a row has a scope, the Portkey package adds
`workspace_slug=<scope name>` to every analytics and groups request, the assertion's included.
Two kinds of key reach it:

*   A **workspace key**. Portkey binds it to one workspace and ignores the slug. This was checked
    live on 2026-11-26 (Q9): a slug naming another workspace, or none, changed nothing. The
    assertion is what catches a workspace key pasted under the wrong target.
*   An **organisation key**. Portkey documents that it reads the workspace the slug names and,
    with no slug, its organisation's default workspace. Because every request carries the slug,
    the default-workspace fallback cannot be reached. Whether every endpoint honours the slug is
    question Q6.

The package does not need to know which kind it holds: the same requests are correct for both.
A legacy row has no scope, so a legacy install sends exactly the requests it sends today.

The Portkey package implements the assertion with a call it already makes for its existing
optional check: it reads the workspace grouping of the analytics API over the last seven days,
with the slug.

*   `Matched`: exactly one workspace was returned and its slug is the configured scope name.
*   `Mismatch`: another workspace, more than one, or a row with no workspace.
*   `Unverifiable`: no workspace had traffic in that window.
*   A failed call: any non-200 status, a response the vendor marks as quota-limited, a response
    that cannot be decoded, or a workspace value that cannot be read. None of these is ever
    treated as a result.

When a scope is set, the Portkey package also refuses at start a loop whose lookback is longer
than six days, which leaves a day's margin inside that window, and refuses the legacy
`settings.expected_workspace` setting.

**The scope gate.** The policy is one object per pipeline. It lives in the source package beside
the capability, as the guard does. It holds one state for the pipeline, in memory: no result,
matched, unverified or mismatched. It makes one assertion call at a time. The re-assertion
interval is a constant, 10 minutes. It is not a config key.

*   **When a replica becomes leader**, before any loop starts, the scheduler resets every gate and
    each gate reads its pipeline's remembered match (section 7) through a checkpoint handle given
    to it at assembly. If one exists and its digest equals the digest of the current key
    fingerprint, scope name and scope id, the gate starts in matched, with its first re-assertion
    due at a random time inside the first interval. Spreading the re-assertions this way keeps a
    restart from sending every tenant's assertion at once. Otherwise, or if the read fails, the
    gate starts with no result.
*   **When a replica stops leading**, every gate is reset to no result. A replica that is not
    leading reports no result.

A mismatch is not remembered across a change of leader. A mismatched pipeline starts with no
result on the next leader, asserts again and reaches mismatched again if the cause remains. Both
states emit nothing, so nothing leaks in between.

The scheduler asks the gate once in every tick of every loop in the pipeline, **before the
collect**, after its existing check that the loop has no batch in flight:

*   Matched, and the re-assertion is not yet due: the tick goes on with no call.
*   Matched, and the re-assertion is due: the gate calls the assertion.
*   No result: the gate calls the assertion.
*   Unverified or mismatched: if the gate called the assertion less than a minute ago, the state
    stands and the tick ends. Otherwise the gate calls it.

There is no assertion after the collect. A batch collected in a matched state is queued as it is
today.

What happens for each outcome:

*   `Matched`: the state is matched, the next re-assertion is due one interval later, and the
    tick goes on. If the pipeline has no remembered match for the current digest, the gate writes
    one (section 7). A write that fails or is refused leaves the in-memory state as it is, because
    that state comes from a fresh assertion, and the write is tried again at the next `Matched`.
*   `Mismatch`: the state is mismatched and the gate writes the remembered match empty. The tick
    ends before the collect. The scheduler puts that loop into the existing degraded state, so it
    is ticked every ten minutes instead of at its cadence. Each of the pipeline's loops goes
    degraded on its own next tick. Only a later `Matched` result ends a mismatch; an
    `Unverifiable` result does not. For a workspace key, that `Matched` comes when the key is
    corrected and the pods restarted. For a key that saw several workspaces, it can also come
    when the other workspaces' traffic has aged out of the seven days. The loop then collects
    again, and its next successful checkpoint save clears its degraded state, as it does today.
*   `Unverifiable` when matched: the state stays matched. A key that has read only this scope
    still does when the scope goes quiet, so an idle workspace goes on emitting its zero-valued
    samples.
*   `Unverifiable` otherwise: the state is unverified. The tick ends before the collect. Nothing is
    queued, no watermark moves and no error is counted. The loop keeps its cadence, so the gate
    asserts again on a later tick, at most once a minute.
*   A failed call when matched: the state stays matched and the tick goes on, for up to two
    intervals (20 minutes) after the pipeline's last successful assertion, or after the replica
    became leader for a remembered match. The re-assertion is due again on the next tick, at most
    once a minute. Past that grace the failed call is handled as in the next case. A real
    `Mismatch` ends the grace at once.
*   A failed call otherwise: the state does not change and the tick ends before the collect. It is
    handled like a failed collect today: lag is recorded, the error is counted and logged against
    the loop whose tick made the call, and the loop retries at its cadence with nothing advanced.

Every failed call counts `scope_assertion_total` with result `error`. The assertion reports a 401
or 403 as a distinct kind of error, and the scheduler then raises the auth-error signal for that
loop. The assertion does not raise it itself.

The grace exists because a matched workspace key cannot have changed workspace: a short outage of
the one endpoint the assertion reads should not stop data. Its cost is that an organisation key
whose slug handling broke during those 20 minutes keeps collecting, which the assertion could not
have seen anyway (Q6).

When the gate ends a tick because the pipeline is unverified or mismatched, the scheduler records
no lag and counts no unreadable time for that tick. When the pipeline resumes, the time that has
become older than its lookback is counted once as unreadable, under the existing skip reason, as
it is after any outage.

**Why one check before the collect is enough.** The first draft of this design asserted again
after every collect (Alternative O). That caught a key that could read a second workspace whose
traffic arrived between the check and the collect. Under the one-key rule such a key is either a
workspace key, which Portkey binds to one workspace, or an organisation key sent one workspace's
slug on every request. Neither reads a second workspace unless Portkey stops honouring the slug,
and an assertion after the collect would not catch that either: it reads only the workspace
grouping, not the graph and group endpoints that carry the data. That case is Q6's.

**What this proves and what it rests on.**

*   A workspace key for the wrong workspace, with traffic in the last seven days, is caught before
    anything is collected. This does not depend on timing.
*   A remembered match is only reused for the same key, scope name and scope id. A key that
    belongs to another workspace has a different fingerprint and so never inherits one.
*   An organisation key is correct only if every endpoint used honours the slug (Q6). The
    assertion sees the workspace grouping only. Nothing in the binary detects another endpoint
    ceasing to honour it between runs of Q6.
*   One case returns `Matched` when it should not: a key from another Portkey organisation whose
    workspace has the same slug, if slugs are unique only inside an organisation (Q9). The
    workspace grouping returns no id to compare.
*   The main defence is still how teams issue keys: a workspace key, limited to reading analytics
    (see Risks).

**Cost.** One assertion per pipeline per re-assertion interval. After a change of leader, a
pipeline with no remembered match asserts on its first tick; one with a remembered match
asserts once inside the first interval, at a random time. A tenant with the two default loops at a
60-second cadence makes 7 requests a minute and one assertion every ten minutes. A waiting
pipeline makes at most one request a minute.

**What the operator documentation states.**

*   A tenant onboarded before its workspace has traffic emits nothing until traffic arrives.
*   After its first match, a tenant goes on emitting through idle spells and restarts until its
    key or its scope changes. A rotated key has a new fingerprint, so it is checked afresh and
    emits nothing until its workspace has traffic.
*   Until a mismatch ends nothing is emitted for that pipeline, and data older than the lookback
    when it ends is lost and counted. The mismatch alert pages, so this loss is not silent.

In the tenant shape the gate is mandatory. The legacy shape keeps its existing optional per-loop
check (`settings.expected_workspace`), untouched, because G6 requires the legacy path not to
change. Two mechanisms therefore exist in the code. At most one is active for any pipeline: a
legacy row has no scope and so no gate, and a row with a scope refuses the legacy setting.

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
*   A fake source returns a distinct marker for each API key and scope, and records the
    `workspace_slug` of every request. A fake OTLP receiver records payloads by credential.
*   **The delivery check:** every payload received under a credential carries only that tenant's
    marker and that tenant's pipeline id.
*   **The slug check:** every request a tenant-shape source sends carries its own target's scope
    name.
*   Both checks must hold through each of these: a payload large enough to be split before sending;
    a payload the receiver refuses as too large, which is split and resent; a retried send; a
    change of leader; a restart with the plan rows supplied in a different order (and, once the
    tenant config exists, with the tenants listed in a different order in the config file); and a
    removed tenant whose target name is then used by another tenant.
*   The self-telemetry endpoint receives no product telemetry, and no tenant destination receives
    a request carrying the self-telemetry identity.
*   Seeded faults: the same check runs against pipelines that are deliberately mis-assembled
    through a test-only path (two pipelines' emitters swapped, two API keys swapped, a source
    that returns another scope's marker, a source that omits the slug and is answered with a
    default scope's marker), and one config with the same API key in two tenants, which must leave
    both unbuilt. The test asserts that the checks report each one. A check that cannot fail
    proves nothing.

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

#### 5.5 One API key per target

The rule that one key serves one workspace is enforced, not assumed. When a pod reads the
tenants' secrets (assembly pass 1), it computes each target's key fingerprint. Every target whose
fingerprint another target also has is a duplicate, whether the two are in one tenant or in two.

*   The tenant of every duplicate target is not built. A tenant with one duplicate target loses
    all its targets, because a key the operator configured twice is a mistake whose correct half
    is unknown.
*   `tenant_blocked` with reason `duplicate_key` is 1 for each such tenant, and the tenant-blocked
    alert pages.
*   One log line per duplicate set names the pipeline ids that share a key. It never names the
    key or its fingerprint.
*   Every other tenant is built and runs. If every tenant is blocked the process does not start.

The config check never reads secrets, so it cannot find a duplicate. A legacy install is not
checked: it serves one team, and G6 keeps its startup unchanged.

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
existing name. The lease keeps the shared Kubernetes client; Q5 was not measured.

**Remembered scope match.** Each pipeline has one more checkpoint key: its source instance, the
reserved loop name `scope-match` and the fingerprint field `v1`. No vendor loop may use that
name; assembly refuses one that does. Its record is an ordinary watermark saved through the
existing store, so it is fenced by leader number like any other:

*   `Time` is the later of the current time and one second after the stored record's time. The
    store refuses a record older than the one it holds, so a new leader whose clock is behind the
    last one's could otherwise never write it, or clear it.
*   `Cursor` is the SHA-256, in hex, of the key fingerprint, the scope name and the scope id,
    joined with a NUL byte. The load rules refuse a scope name or id that contains a control
    character, so the join is unambiguous. An empty `Cursor` means no match is remembered.
*   It is written when the assertion returns `Matched` and the stored digest differs from the
    current one, and written empty on every `Mismatch` whose stored digest is not already empty,
    so a clearing write that failed is retried. A pipeline at steady state writes
    it once, not every interval.
*   The digest cannot be turned back into the key. Anyone who can read the checkpoint ConfigMap
    can test a guessed key against it, but an API key is a long random string.

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
    identical to today's: `source` and `loop`, with no `tenant` or `pipeline`.
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
        `unverifiable` or `error`. It moves each time the gate calls the assertion: when a
        pipeline with no remembered match starts, at least once per re-assertion interval for a
        matched pipeline, and at least every ten minutes for one that is unverified or
        mismatched.
    *   `tenant_blocked`: a gauge with the labels `tenant` and `reason` and no `pipeline`. It is
        1 for a blocked tenant and 0 for every other tenant. Its reasons are `secret_unreadable`
        and `duplicate_key`. Every replica reports it from its own read of the secrets, since a
        standby may become leader with that view. Replicas started at different times can
        disagree; the alert takes the maximum across replicas, so a block on either pages.
*   Liveness stays one heartbeat for the install. Its threshold is derived from the plan.

**Alerts and dashboards.** The behaviours below are the acceptance criteria. The rule expressions
are written and tested in the change that edits the rule files.

*    Every existing rule that groups by `source` and `loop` also groups by `tenant` and `pipeline`.
    That changes nothing for series without those labels, so a legacy install's rules behave as
    today. This covers the alerts for a stale poller, failing emits, auth errors, data loss, queue
    backpressure, buckets revised after settle and a cardinality spike, and the five recording rules
    built on the time of the last success.
*   The stale-poller alert judges only pipelines whose state is matched. Series with no `tenant`
    label are judged as today.
*   The leader-absent alert is unchanged. A new install whose tenants have never matched records
    no success and fires it; the onboarding documentation says so. Once a pipeline has matched it
    emits through idle spells, so this does not recur after restarts.
*   New alerts: scope mismatched for 5 minutes (pages); tenant blocked for 5 minutes (pages);
    scope assertion returning only errors for 30 minutes (warning), which covers a pipeline
    stuck with no result; and unverified for 24 hours (low severity).
*   The self-telemetry dashboard gains tenant and pipeline filters and a scope state panel.

### 9. Failure handling

Every signal in a "Run" row carries `tenant` and `pipeline`.

| Failure | Phase | Behaviour | Signal or outcome |
| --- | --- | --- | --- |
| A broken load-phase or resolve-phase rule | Load, resolve | The process does not start. The config check finds it in CI. | Exit. The new pod never becomes ready, so the rollout stops and the old pods keep running. |
| A broken assemble-phase rule | Assemble | The process does not start. | The same. The config check finds every one that needs no secret. |
| A tenant's secret file is missing or unreadable | Assemble | That tenant's pipelines are not built. Every other tenant starts. If every tenant is blocked, the process does not start. | `tenant_blocked` with reason `secret_unreadable` |
| 401 or 403 from the vendor, on a collect or on the scope assertion | Run | That loop does not advance and retries at its cadence, as today. | `auth_errors_total` |
| The scope assertion call fails for another reason | Run | The tick ends before the collect. The loop retries at its cadence. | `emit_errors_total` with kind `collect`; `scope_assertion_total` with result `error` |
| Scope mismatch | Run | Nothing collected; the remembered match is cleared. Each loop is retried every ten minutes until a `Matched` result. | `scope_state`; `loop_degraded` with reason `scope_mismatch` |
| Scope unverifiable, never matched | Run | Nothing collected, no watermark moves. Asserted again on a later tick, at most once a minute. | `scope_state` |
| Scope unverifiable, already matched | Run | The pipeline stays matched and keeps emitting. | `scope_assertion_total` with result `unverifiable` |
| Two targets share an API key | Assemble | Both tenants are not built. Every other tenant starts. | `tenant_blocked` with reason `duplicate_key` |
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

*   Its checkpoint keys change, so every loop starts from its `bootstrap_lookback` and sends the
    last settled buckets again. With an unchanged identity the backend treats them as repeats.
    With a new identity they are new series.
*   An idle workspace emits nothing until its first match under the new keys, then emits zeros
    through idle spells as before.
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
in-memory assertion result. A pipeline with a remembered match resumes matched; any other asserts
again before its first collect. A rollout also clears the in-memory record
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
| A workspace key for the wrong workspace delivers another team's data. | The scope assertion before the first collect and every re-assertion interval. The pipeline attribute as evidence. | The assertion sees traffic, not permissions: a wrong key whose workspace has been idle for seven days stays unverified and emits nothing until that workspace has traffic, and is then caught. |
| An organisation key reads more than its target's workspace. | The slug on every request; Q6 for every endpoint used, run again for each version that changes the Portkey package; the scope assertion. | The assertion reads one endpoint. Nothing detects another endpoint ceasing to honour the slug between runs of Q6. |
| An organisation key is a credential for the whole organisation. | Teams are asked for a workspace key first. An organisation key should be limited to `analytics.view` where Portkey allows it (Q6). | A leaked organisation key exposes every workspace's analytics, and more if its scopes cannot be limited. |
| One API key is configured for two tenants. | The duplicate-key check at assembly (section 5.5), which blocks both and pages. | Found at pod start, not in CI. |
| A bug pairs a loop with the wrong emitter. | One place of assembly; the release gate with seeded faults. | A test proves the code against a fake, not against production. |
| One process holds every team's source API key and destination token. | Secrets are files in one directory per tenant and are never placed in the config tree, so a config error cannot contain one. Each emitter already removes its own credentials from error text. Teams should issue a destination token limited to writing metrics. | Anyone who can read Secrets in the install's namespace, or exec into its pods, can read every tenant's credentials. Access to that namespace is the central team's alone. The author accepted this. |
| A restart or a bad version pauses every tenant in the install. | Recovery is from checkpoints with no gap inside the lookback; `maxUnavailable: 0`; a ceiling of about 25 tenants per install. | A pause of up to a minute per rollout. A tenant that needs more isolation gets its own install (Alternative B). |
| The vendor's request budget is shared across keys, or smaller than the design's load. | Not documented (Q1). The per-target rate limiter and the scheduler's quota backoff. Cadence is the first control: one analytics request returns up to 55 one-minute buckets, so a five-minute cadence makes about a fifth of the requests with no loss of data. | If the shared budget supports fewer than 20 tenants the design is stopped. |

| Every checkpoint save rewrites one ConfigMap. | Estimated at under one save a second at 20 tenants, not measured (Q5). Alternative L is the first response. | The store holds every key ever written until an offline prune exists. |
| The change breaks an existing single-team install. | Golden tests and an upgrade test land first and stay green through every change. Changes 2 to 5 do not alter a legacy install's emitted telemetry, checkpoint keys or self-metric label sets. | A legacy install with two sources sharing a loop name keeps today's overwritten lag gauge. |
| A newly added tenant, or one with a rotated key, looks broken because nothing is emitted. | An explicit unverified state, left out of outage alerts, with its own low-severity alert; stated in the tenant data contract. | A stack shows no data, not zeros, until the workspace's first traffic under the current key. |
| Every tenant's assertion reads one endpoint Portkey does not document (`/analytics/groups/workspace`). | Matched pipelines ride out up to 20 minutes of failures (section 5.1). The stale-poller alert fires for every matched pipeline past that. | A change or removal of that endpoint stops every tenant at once, and no other endpoint is known to replace it. |
| Two tenants name the same workspace with an organisation key. | No scope may appear in two tenants, checked by the config check in CI (section 2). | An organisation key can still be pointed at a workspace that belongs to no tenant of the install; only review of the plan table catches that. |
| Workspace slugs are unique only inside a Portkey organisation (Q9). | None in the binary: the workspace grouping returns no id. | A key from another organisation whose workspace has the configured slug returns `Matched`. Accepted if Q9 finds slugs are not globally unique. |
| A rule that needs a tenant's secret is broken (a duplicate key or an unreadable secret), which the config check cannot find. | Only the tenants concerned are blocked, and the alert pages. A process-stopping assemble rule is found by the config check in CI. | The mistake is found at rollout, not in CI. Config revisions are immutable, but a replacement pod can still see updated Secret values. |

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

### Alternative H: One organisation-level API key for every workspace

One organisation key, with one target per workspace, each sending its own `workspace_slug`.

*   **Pros**: One credential for a whole organisation. Portkey documents `workspace_slug` as
    selecting the workspace for an organisation key, so it could route per workspace. (The first
    draft rejected it as unable to route; that rested on a probe made with a workspace key.)
*   **Cons**: One revocation stops every tenant. Every tenant shares one key's request budget. An
    organisation-wide credential serves teams that should each hold their own.
*   **Reason for Rejection**: Rejected by author on 2026-11-26: one API key serves exactly one
    workspace.

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
*   **Reason for Rejection**: Rejected after research (Q2): cloud availability is not documented,
    failed exports are lost, and the trace export carries prompt content.

### Alternative O: Assert again after every collect, before the batch is queued

The first draft's design: an assertion before the collect, and another after it on every tick.

*   **Pros**: Catches a key that can read a second workspace whose traffic arrived between the
    check before the collect and the collect itself.
*   **Cons**: One more request per collect, and a failed assertion throws away a completed
    collect. Under the one-key rule the case it catches needs Portkey to stop honouring the slug,
    and the assertion would not see that, because it reads a different endpoint from the data.
*   **Reason for Rejection**: Rejected by author on 2026-11-26, once a key serves one workspace.

### Alternative Q: Workspace keys only at launch

*   **Pros**: The launch rests only on Portkey binding a workspace key, which Portkey documents
    and Q9 observed. No organisation-wide credential is held.
*   **Cons**: A team that can only issue an organisation key cannot be onboarded.
*   **Reason for Rejection**: Adopted by the author on 2026-11-26 for the launch, with Q6
    deferred. Organisation keys follow once Q6 is run.

### Alternative R: Stop the install on a duplicate API key

*   **Pros**: The mistake cannot be missed.
*   **Cons**: One tenant's config mistake stops every tenant at the next rollout.
*   **Reason for Rejection**: Rejected by author: block the tenants that share the key and page.

### Alternative S: Emit nothing until traffic is seen after every change of leader

The first draft's behaviour for an idle workspace.

*   **Pros**: No state about the scope outlives a leader.
*   **Cons**: Every rollout stops every idle tenant, including its zero-valued samples, until its
    workspace has traffic again.
*   **Reason for Rejection**: Rejected by author: a match is remembered per key and scope.

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
*   Self-telemetry names every tenant and pipeline, and the plan table names every scope. Both
    go only to the central team: self-telemetry to `emit.self`, the plan table to CI output.
*   The config check prints scope names and ids, URLs and instance ids. None is a secret.

**Performance.**

*     Expected steady state at 20 tenants with two loops each at a 60-second cadence: under one
     checkpoint save a second, and between two and three vendor requests a second. After a restart
     each tenant makes 8 requests in its first minute, paced by its own rate limiter. These are
     estimates the author accepted (Q1, Q3, Q5).
*   Each pipeline's guard holds its own seen-values set, up to the cardinality budget per metric
    name. Its footprint at the full budget is not measured (Q3).

**Rollout and rollback.**

*   The work lands as a sequence of changes that do nothing in the legacy shape, then one version
    that accepts the tenant shape. From that version the items in G8 are fixed.
*   Each change before that version can be rolled back by image.
*   A tenant team is told, in a one-page contract: data arrives about ten minutes behind because
    of settle; a gap longer than the lookback is permanent and counted; a rollout of the shared
    install pauses delivery for up to a minute; nothing is emitted before the workspace's first
    traffic under the current key, and after that idle spells are sent as zeros.


## Additional Resources

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
*   Portkey analytics parameters (`workspace_slug`, `include_all_workspaces`,
    `exclude_workspace_slugs`):
    https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/graphs-time-series-data/get-requests-data
    and
    https://docs.portkey.ai/docs/api-reference/admin-api/control-plane/analytics/groups-paginated-data/get-model-grouped-data
*   `internal/source/portkey/AGENTS.md`, `followup.md` and issue #321 say Portkey ignores
    workspace targeting. That holds for a workspace key, which is what was probed. It is not
    known for an organisation key until Q6.

## Future Improvements

*   **Shards**: section 12.
*   **Re-reading a tenant's secret file on an auth failure** (Alternative M).
*   **A way to tell which scope an API key belongs to that does not need traffic.** It would let
    a new tenant emit before its first traffic. Q9 showed a wrong slug is not refused, so this
    needs another vendor endpoint.
*   **A standing check of the vendor behaviour in Q6**: a target on an organisation key with a
    slug for a workspace that has traffic but is not its tenant's, checked against a workspace key
    for that workspace, with an alert if the two ever differ.
*   **Scope discovery as a detector**: list scopes, compare with the plan, and signal the ones
    with no target. An operator adds the target.
*   **Log records in the tenant shape**, after Q7.
*   **Per-API-key use-case labels on a target.**
*   **Per-tenant governance settings.**
*   **LangSmith in the tenant shape**, which needs a scope assertion for that source.
*   **Retiring the legacy `expected_workspace` check**, by giving the legacy shape an optional
    scope that uses the gate.
*   **An offline prune of orphaned checkpoint keys.**
*   **A Kubernetes operator and a tenant resource**, only if an outside system must add tenants
    without a reviewed config change.

## Order of work

The work lands in this order. Each unit is one issue and one pull request: #520 to #536. Units
3b, 4, 5b, 6b and 6e are each split into two issues (for example 3b-1 and 3b-2). The
step-by-step plan is not part of this document: it is written into the issues.

| Unit | What it is | Implements | Needs first |
| --- | --- | --- | --- |
| 1 | Compatibility harness, tests only | Section 10, goal G6 | Nothing. Can start at once. |
| 2 | Tenant and pipeline labels in self-telemetry | The labels and log attributes of section 8, goal G4 | Unit 1 |
| 3a | The plan package and the config check's output | Section 3 | Unit 2 |
| 3b | The three passes of assembly and the two factories | Sections 4, 5.2 and 6 | Unit 3a |
| 4 | The scope gate, the slug on every request and the remembered match | Sections 5.1 and 7 and their signals in section 8 | Unit 3b |
| 5a | The race detector on the acceptance recipe | Verification | Unit 3b |
| 5b | The release gate and the isolation tests | Section 5.3, goals G2 and G5, the run-time cases of G3 | Units 4 and 5a |
| 6a | The tenant config schema and its load rules | Section 2 (load phase), goal G9 | Unit 3b |
| 6b | Resolving, secrets, the duplicate-key check and assembly for tenant rows | Sections 2 (resolve and assemble phases), 5.5, 7 and 9, goal G8 | Units 5b and 6a |
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
