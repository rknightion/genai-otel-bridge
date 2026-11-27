# Multi-tenancy

From the version that enables the tenant shape, one install can serve several teams (tenants). Each
tenant's data goes to its own Grafana Cloud stack, with its own credentials and identity. An older
image refuses a config file with a `tenants` map at startup, so upgrade the image before you change
the config ([Converting a legacy install](#converting-a-legacy-install)). The design and its reasoning are in
[`docs/designs/multi-tenancy.md`](https://github.com/rknightion/genai-otel-bridge/blob/main/docs/designs/multi-tenancy.md).

```yaml
identity:                               # the install's own identity, used for self-telemetry
  service_namespace: genai-otel-bridge
  deployment_environment: prod

ha: { coordinator: lease, checkpoint: configmap }
queue: { emit_workers: 1 }

emit:
  self:                                 # where self-telemetry goes; never a tenant's destination
    otlp:
      endpoint: ${GC_OTLP_ENDPOINT}
      instance_id: ${GC_INSTANCE_ID}
      token: ${GC_OTLP_TOKEN}

defaults:
  portkey:                              # keyed by source type; every target of the type uses it
    loops:
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

tenants:                                # a map keyed by tenant id
  team-a:
    identity:                           # required; stamped on this tenant's product telemetry
      service_namespace: team-a-llm
      deployment_environment: prod
    destinations:                       # a map keyed by name, local to this tenant
      main:
        otlp:
          endpoint: https://otlp.example.com/otlp
          instance_id: "100001"
          token_file: otlp-token        # a file name in this tenant's secrets directory
    targets:                            # a map keyed by name, local to this tenant
      prod:
        source:
          type: portkey
          base_url: https://api.portkey.ai/v1
          auth: { header: x-portkey-api-key, value_file: source-key }
          rate_limit: { rps: 1, burst: 3 }
        scope:                          # required; the one workspace this key reads
          name: team-a-workspace-slug
          id: team-a-workspace-id
        destination: main
```

Tenant, destination and target names match `[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?`. The tenant shape
needs `ha.coordinator: lease` and `ha.checkpoint: configmap` (set in the block above), so the ECS and DynamoDB deployment
does not support it. Log-record loops (`logs_export`) are refused in the tenant shape: the launch
emits metrics only. The load and resolve rules are listed in
[Configuration reference](./config-reference.md#tenant-shape). The chart renders the config from
`config.tenants` and `config.defaults`, or from `configOverride`; the worked example is
`deploy/helm/examples/tenants-values.yaml`.

**Workspace keys only.** Every target's API key must be a workspace-level key limited to one
workspace. Organisation-level keys are not supported. Whether every analytics endpoint honours the
workspace slug for an organisation key is still an open question, Q6 of the design, and organisation
keys follow once it has been run (see [below](#what-the-scope-assertion-covers-and-re-running-q6)).

## Adding, changing and removing a tenant

1. Create the tenant's Secret (see [Secrets and the restart command](#secrets-and-the-restart-command)).
2. Add the tenant under `config.tenants` in the chart values, with its identity, destination and
   targets. A tenant's stack must be its own: two tenants may not share a stack, and no scope name
   or id may appear in two tenants.
3. Run the config check on the new image and review the plan table (see
   [The config check and plan table](#the-config-check-and-plan-table)).
4. Roll the install. Every pipeline pauses for one or two leadership handovers and resumes from its
   checkpoints with no counted skips (see
   [Rollouts and rolling back a stopped one](#rollouts-and-rolling-back-a-stopped-one)).
5. The new pipeline asserts its scope before its first collect. Its metrics appear in its stack once
   the first buckets are older than `bucket_settle`, about ten minutes later (see
   [Idle and mismatched tenants](#idle-and-mismatched-tenants)).

Changing a tenant is a config change and goes through the same steps. Changing a tenant's identity
starts new series in its stack. Changing a target's destination or scope keeps its watermark: the new
destination or scope receives data from the watermark onward and nothing earlier is sent again. To
start a fresh history, use a new target name (see [Renames](#renames)).

Removing a tenant stops its pipelines at the next rollout. Its data stays in its stack and its old
checkpoint keys stay in the checkpoint ConfigMap. Delete its Secret once the rollout has finished.

A tenant's API key, token and identity are the tenant's own. A tenant of one install cannot name
another tenant's objects or secret files: a secret file is a bare file name read from that tenant's
directory only.

## Secrets and the restart command

No secret goes in the config. A tenant's secrets are files in a Kubernetes Secret named
`genai-otel-bridge-tenant-<tenant id>`. The chart mounts one Secret per tenant, read-only and
optional, at `/var/run/secrets/genai-otel-bridge/tenants/<tenant id>/`. `token_file` on a destination
and `value_file` on a target name keys of that Secret. Using the file names of the example:

```bash
kubectl -n <namespace> create secret generic genai-otel-bridge-tenant-team-a \
  --from-file=otlp-token=<path to the token file> \
  --from-file=source-key=<path to the API key file>
```

The install's own Secret, which holds the self-telemetry credentials, is still required. A
tenant's Secret is optional: a missing one does not stop the pod, it blocks that tenant.

Secrets are read once, when the pod starts. They are not re-read while the pod runs, and the chart
restarts pods only when the config changes, so **changing a Secret does not restart anything**.
After you change a tenant's key or token, or create a missing Secret, restart the pods:

```bash
kubectl -n <namespace> rollout restart deployment/genai-otel-bridge
kubectl -n <namespace> rollout status deployment/genai-otel-bridge
```

Two cases block one tenant and leave every other tenant running. Each sets
`genai_otel_bridge_tenant_blocked{tenant, reason}` to 1 and pages
([GenaiOtelBridgeTenantBlocked](./alerts.md#genaiotelbridgetenantblocked)):

- `secret_unreadable`: a secret file of the tenant is missing, empty or too large.
- `duplicate_key`: two targets, in one tenant or in two, use the same source API key. Both tenants
  are blocked, because the correct half of a duplicated key is unknown. The log line names the
  pipeline ids that share a key and never the key.

If every tenant is blocked the process does not start. A blocked tenant has no loops and so no lag
series: alert on the blocked gauge, not on staleness. Its metrics have a permanent, counted gap
once the block outlasts the lookback, which is why the alert pages after five minutes.

## The config check and plan table

The config check runs the new image over the config file with no secrets and no network:

```bash
genai-otel-bridge -validate-config -config <config file>
```

It prints the plan table, one row per target, then `validate-config: OK (<path>) plan <hash>`. Run
it in CI before a config change merges, and review the table as a diff: each row shows the tenant,
target, source URL, scope name and id, pipeline id, destination name, destination endpoint and
instance id, identity and loops. The destination columns are where every target is sent, so a
tenant pointed at the wrong stack is visible in review. The table never prints a secret.

The hash is the same one the leader logs each time a replica becomes leader, with the row count.
Match the two to confirm a running install uses the plan that CI printed. The layout of the table
is not a contract across versions, so compare hashes only between CI and an install on the same
image.

The check finds every rule that needs no secret. It cannot find an unreadable secret or a duplicate
key, because it never reads secrets: those are found when the pod starts.

## What the scope assertion covers and re-running Q6

Before a pipeline's first collect, and again every ten minutes, the bridge asks the vendor which
workspaces the key saw traffic in over the last seven days. For Portkey that is the workspace
grouping with the target's slug. One workspace equal to the configured scope name is a match.
Another workspace, several, or a row with no workspace is a mismatch. No traffic at all is
unverifiable. A failed call is never treated as a result.

What it proves, and what it does not:

- A workspace key pasted under the wrong target, whose workspace has had traffic in seven days, is
  caught before anything is collected. This does not depend on timing.
- A remembered match is reused only for the same key, scope name and scope id, so a different key
  never inherits one.
- It sees which workspaces had traffic, not what a key is allowed to access. A wrong key whose
  workspace has been idle for seven days stays unverified and emits nothing until that workspace has
  traffic, and is then caught.
- It reads one endpoint. Nothing detects another endpoint ceasing to honour the slug.
- If the assertion endpoint, which Portkey does not document, changes or is removed, every matched
  pipeline rides out 20 minutes of failed calls and then the stale-poller alert fires for each.

The main defence is how teams issue keys: a workspace key limited to reading analytics (see
[Least-privilege keys](#least-privilege-keys)).

**Re-running Q6.** Organisation keys stay unsupported until a person has run Q6 with an organisation
key over two workspaces that both have traffic: send each analytics and groups endpoint the slug of
one workspace and confirm only that workspace's rows come back. The bridge already sends
`workspace_slug` on every request in the tenant shape, so a pass needs no code change. Run it again
for each version that changes the Portkey package. A workspace key ignores the slug (probed on
2026-11-26: no slug, the right slug, a slug naming no workspace and the all-workspaces parameters
returned the same rows), which is why the assertion, not the slug, is the control for workspace
keys. Until then, a target configured with an organisation key is outside what this version
supports, even where the assertion happens to pass.

**Residual risk of a stale match.** A mismatch writes the remembered match empty. If the checkpoint
store fails both that clearing write and the re-read across a mismatch, and leadership then
changes, a new leader can restore the stale match and start the pipeline matched. It re-asserts
within the first ten minutes and returns to mismatched if the cause remains, so the exposure is up
to ten minutes of collection by a key that no longer matches. A scope mismatch alert that fired
earlier is the sign to look for.

## The slug residual risk

The assertion compares slugs, because the workspace grouping returns a slug and no id. Portkey does
not document whether a slug is unique across organisations, and its documented slugs carry a random
suffix. A key from another Portkey organisation whose workspace has the configured slug would
return a match. The binary has no defence against this: the accepted residual risk is that the key
was issued by the team that owns the tenant, which is why the central team creates or reviews every
key. Q9 of the design records this as accepted.

## Idle and mismatched tenants

A pipeline's scope gate has one state per pipeline, shown by `genai_otel_bridge_scope_state`
(`matched`, `unverified` or `mismatched`; all 0 on a replica that is not leading).

- **Onboarded before its workspace has traffic.** The state is `unverified`: nothing is emitted, no
  watermark moves and no error is counted. It asserts again on a later tick, at most once a minute,
  and starts by itself once traffic arrives. A stack shows no data, not zeros, until then.
  [GenaiOtelBridgeScopeUnverified](./alerts.md#genaiotelbridgescopeunverified) is a low-severity alert after 24 hours.
- **After its first match.** The match is remembered for that key and scope, so a tenant goes on
  emitting zero-valued samples through idle spells and restarts. A rotated key has a new
  fingerprint, so it is asserted afresh and emits nothing until its workspace has traffic.
- **Mismatched.** Nothing is emitted. Each loop is retried every ten minutes until a match.
  [GenaiOtelBridgeScopeMismatched](./alerts.md#genaiotelbridgescopemismatched) pages. Only a later match ends it: for a workspace key, correct the key
  and restart the pods. Data older than the lookback when it ends is lost and counted under
  `samples_skipped_total`, so this loss is not silent.
- **Assertion failing.** A pipeline with no result and failing calls (for example a revoked key)
  shows as scope assertion errors for 30 minutes
  ([GenaiOtelBridgeScopeAssertionErrors](./alerts.md#genaiotelbridgescopeassertionerrors)). A 401 or 403 also counts as an auth error.

On a new install whose tenants have never matched, nothing has recorded a success yet, so
[GenaiOtelBridgeLeaderAbsent](./alerts.md#genaiotelbridgeleaderabsent) still fires. That is expected: it clears after the first
match, and does not recur after restarts because a matched pipeline emits through idle spells.

## Renames

A rename is not blocked. A renamed tenant or target gets a new pipeline id and a new checkpoint key,
so it starts new watermarks from `bootstrap_lookback` and leaves its old checkpoint keys behind.
Alerts, recording rules and dashboard series follow the new `tenant` and `pipeline` labels. If a
stack turns the pipeline attribute into a label, every series of that pipeline there becomes a
new series.

The store has no delete and refuses writes above 900 KiB, roughly 3,600 keys, so orphaned keys
count against that limit (an offline prune is future work). A tenant id that is removed and later
added again picks up its old watermark, and the time since then that is older than the lookback is
counted as skipped.

The checkpoint key contains the source instance `mt.<tenant>.<target>` and no destination or scope.
A legacy `source_instance` that begins with `mt.` could collide with a tenant's keys when the
install is converted, and the binary logs a warning at start when one does.

## Rollouts and rolling back a stopped one

A rollout pauses every pipeline for one or two leadership handovers of about 20 to 30 seconds each
(the 15 second lease, the 2 second retry period and a first collect). Each pipeline then resumes
from its checkpoints. A rollout also discards the in-memory scope result: a pipeline with a
remembered match resumes matched and the rest assert again before their first collect. The chart
needs `rollingUpdate.maxUnavailable: 0` for any tenant and refuses to render otherwise, so a new pod
must be ready before an old one is removed, and a pod that fails to load or assemble its config
never becomes ready. Run two replicas, a leader and a hot standby. About 25 tenants per install is
the planned ceiling.

When a new pod cannot start, the rollout stops and the old pods keep serving. A replacement pod
from the old ReplicaSet mounts its original [config revision](./installation.md#config-revisions),
not the bad config. Secret values can still change independently. Restore the last good tenant
config and compatible image to regain redundancy. Do not roll back to a version from before the
tenant shape: its binary does not know the config keys.

Load, resolve and most assemble rules stop the process and are found by the config check in CI. An
unreadable secret and a duplicate key are not: they block one tenant and are found at rollout.

## A narrowed NetworkPolicy

The chart's NetworkPolicy has one OTLP egress range, `networkPolicy.otlpEgressCIDR`, which defaults
to everything and is not changed by tenants. An install that has narrowed it must cover every
tenant's destination, and every target's source host through `sourceEgressCIDR`. A missing range
shows as that tenant's emits failing while others succeed.

## Converting a legacy install

Converting a running legacy install to the tenant shape is a migration, not an upgrade. Do it in
two steps:

1. Upgrade the image with the legacy config. This is an ordinary upgrade: checkpoint keys, emitted
   bytes and self-metric labels are unchanged, and a rollback to the previous image works.
2. Change the config to the tenant shape and roll again.

Do not skip step one: verify the new binary on the current shape before changing the config and
checkpoint keys. After step two the checkpoint keys change, so every loop starts from
its `bootstrap_lookback` and sends the last settled buckets again. With an unchanged identity the
backend treats them as repeats. With a new identity they are new series. An idle workspace emits
nothing until its first match under the new keys, then emits zeros through idle spells as before.
Replace `settings.expected_workspace` with the target's `scope`: the tenant shape refuses the
legacy setting.

## The tenant data contract

What a tenant can rely on in its own stack:

- **Metrics only.** Log records are not exported in the tenant shape.
- **Its identity.** Every product series carries the tenant's `service.namespace`,
  `service.name` and `deployment.environment` (or a target's own identity). On Grafana Cloud these
  form the `job` label, and the environment is promoted by default; check the stack's promoted set
  when onboarding.
- **The pipeline attribute.** Every resource carries `genai_otel_bridge.pipeline`, whose value is
  `<tenant>.<target>`. It is kept on `target_info` unless the stack promotes it to a label. It is
  evidence, not routing.
- **Settled data.** Buckets appear after `bucket_settle`, about ten minutes behind real time, and
  are sent once, in a byte-identical form.
- **No data before first traffic.** A new tenant, or one with a rotated key, shows no data (not
  zeros) until its workspace has traffic. After that, idle spells emit zeros.
- **Gaps are counted.** A mismatch, a block or a long outage past the lookback ends in a counted
  skip, never silently.
- **Its health signals are the install's.** Self-telemetry goes to `emit.self`, never to a
  tenant's stack. Delivering them to a tenant is not part of this version.
- **Stable formats.** The checkpoint key format, the pipeline id format, the name of the pipeline
  attribute, the `tenant` and `pipeline` self-metric labels and the tenant config keys do not
  change after the first tenant-shape version.

To look for another tenant's data in your stack, query `target_info` for any pipeline id that is
not yours. For a tenant `team-a` the query is:

```promql
count by (genai_otel_bridge_pipeline) (
  target_info{genai_otel_bridge_pipeline!="", genai_otel_bridge_pipeline!~"team-a\\..+"}
)
```

It must return nothing. The label name assumes the stack converts the attribute dots to underscores,
as Grafana Cloud does, and that it keeps the attribute; confirm both for your stack first. The
bridge side of the same check is `genai_otel_bridge_emitted_total` by `tenant` and `pipeline`.

## Least-privilege keys

One process holds every tenant's source API key and destination token. Anyone who can read Secrets
in the install's namespace, or exec into its pods, can read all of them, so access to that namespace
belongs to the central team alone.

- **Source keys.** Ask each team for a workspace-level key for its one workspace, limited to reading
  analytics. Organisation keys are not supported (see the top of this page). One key serves one
  target: a key configured twice blocks both tenants.
- **Destination tokens.** Issue a token limited to writing metrics for that stack.
- **Every credential is a file** in the tenant's own Secret and never in the config tree, so a config
  error cannot contain one. Emitters remove their own credentials from error text.
