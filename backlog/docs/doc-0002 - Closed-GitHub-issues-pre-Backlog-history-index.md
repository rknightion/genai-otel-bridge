---
id: doc-0002
title: Closed GitHub issues (pre-Backlog history index)
type: other
created_date: '2026-08-14 16:07'
updated_date: '2026-08-14 16:07'
---
> **Historical index of work tracked on GitHub Issues before this repo moved to Backlog.md on
> 2026-08-14.** The closed issues were **deleted from GitHub** on that date, so `gh issue view <N>`
> will 404. Their full bodies and all comments are archived in
> `archive/github-issues-2026-08-14.json` — **that file is the record; this table is the index into
> it.** The load-bearing detail (failure scenario, evidence, acceptance criteria, the reasoning that
> settled a design argument) is in the bodies, so read the archive, not just this table:
>
> ```sh
> jq '.[] | select(.number == 142)' archive/github-issues-2026-08-14.json
> ```
>
> The archive is **unredacted**, because a per-field sweep of 1,934 decoded fields found nothing
> requiring redaction. `archive/README.md` records what was hunted for and the three benign hits.

**Why these were not imported as tasks.** Backlog IDs follow creation order, so an imported task
could never carry the number the history already cites — `gob-0042` for `#42` is a second ID space
over the same history. `AGENTS.md`, `ARCHITECTURE.md`, `docs/DESIGN.md`, `followup.md`, the commit
log and the `*_review_test.go` regression guards all reference this work as `#NNN`; keeping the
GitHub numbers as the *only* ID space is what keeps those references resolvable. A hundred-odd
`Done` rows would also drown the board's one real signal — what is left. **Cite closed work as
`#NNN`; cite new work as `gob-NNNN`.**

**All 117 closed on 2026-07-03**, in a single sweep at the end of the production-readiness campaign
(`#142` was its master tracker). The date column is therefore uniform; it is kept per-row so the
table stands on its own rather than on this sentence.

**The commits column is every commit whose message cites the issue**, newest first, capped at three.
It is a lead, not a verdict — a commit may cite an issue it only touches. **The squash-merge suffix
`(#NNN)` on a subject line is a PULL REQUEST number and was stripped before matching**; without
that, every Renovate PR resolves as an issue fix, because PRs and issues share one number space.

**116 of 117 rows resolved to at least one commit, and the correspondence is exact:** the 116
`COMPLETED` issues all resolved, and the single row with no commit — `#132` — is the single
`NOT_PLANNED` one, closed as a deliberate non-change rather than left undone.

| # | Title | Closed | Commits citing it |
|---|---|---|---|
| 26 | Docs site: redesign & rebrand alignment + SEO/LLM discoverability | 2026-07-03 | `0a390df` `59cdd5f` `7c43023` |
| 29 | Emit HTTP client follows redirects: a 301/302/303 converts the OTLP POST into a body-less GET, and a 2xx from the redirect target is recorded as a successful emit — permanent silent data loss with watermark advance | 2026-07-03 | `b11b232` |
| 30 | Bound the DynamoDB coordinator's renew call: a stalled UpdateItem never cancels leaderCtx, giving an unbounded dual-leader window | 2026-07-03 | `a17b740` |
| 31 | ECS task EXECUTION role is never granted secretsmanager:GetSecretValue, so every task launch fails when secret_arns is set | 2026-07-03 | `8b85de9` `9eb656f` |
| 32 | Published Helm chart vN default-installs the vN-1 image (values.yaml pins the pre-release :latest digest), and strict config decode turns any new-config-key release into a default-install crash loop | 2026-07-03 | `8b85de9` |
| 33 | cleanup's always-delete-lease + retainCheckpoint permanently fences the checkpoint after reinstall (watermark stall + duplicate log re-emits) | 2026-07-03 | `90bc41a` |
| 34 | Redact the presigned signed-URL (bearer credential) from Portkey logs_export download transport errors before they reach logs | 2026-07-03 | `a962763` |
| 35 | logs_export permanently wedges on a single JSONL line exceeding maxLogLineBytes — docs claim over-long lines are skipped | 2026-07-03 | `fb09f70` `a962763` |
| 36 | Image tag scheme silently changed from :vX.Y.Z to :X.Y.Z by the container-publish migration; install docs still teach the v-prefixed tags | 2026-07-03 | `19d0612` |
| 37 | Fix SECURITY.md supported-versions table: advertises 2.x but the project is 3.x | 2026-07-03 | `45c97b9` `1656254` `c67e422` |
| 38 | Negative durations (bucket_settle, max_backfill, bootstrap_lookback) pass validation; negative bucket_settle emits unsettled buckets, breaking emit-once-after-settle | 2026-07-03 | `96332f2` |
| 39 | Omitted/zero rate_limit passes validation but makes every upstream request fail instantly (burst 0) | 2026-07-03 | `8e721fd` `fcdef67` `07c2973` |
| 40 | Typo'd/unknown loop names under sources[].loops are silently ignored — no validation error, no warning | 2026-07-03 | `609112c` |
| 41 | emit.self block with empty otlp.endpoint passes validation but breaks the entire self-telemetry plane at runtime | 2026-07-03 | `96332f2` |
| 42 | Public docs never document the ha.dynamodb.* config block or the dynamodb enum value for ha.coordinator/ha.checkpoint | 2026-07-03 | `c67e422` |
| 43 | README.md and docs/installation.md Quickstart examples use bucket_settle/max_backfill values internal docs call out as superseded | 2026-07-03 | `c67e422` |
| 44 | docs/alerts.md references the wrong (untranslated) metric name for queue depth | 2026-07-03 | `c67e422` |
| 45 | Switching ha.coordinator to none over a surviving checkpoint permanently fences all watermark writes (Noop epoch is a constant 1) | 2026-07-03 | `83cf358` `a17b740` |
| 46 | Validate ha.dynamodb.retry_period against renew_deadline/lease_duration — retry >= lease guarantees a recurring split-brain window | 2026-07-03 | `96332f2` |
| 47 | Add kind="unknown_4xx" to the GenaiOtelBridgeEmitFailing alert regexp — the terminal-halt kind the taxonomy promises to alert on is the one kind the alert misses | 2026-07-03 | `8b85de9` |
| 48 | ECS README tells operators to tune a nonexistent `emit.retry_max` config knob — following the advice makes the binary fail config load (strict decode) | 2026-07-03 | `c67e422` |
| 49 | ExternalSecret renders empty remoteRef keys for any unset externalSecrets.* value — Portkey-only or LangSmith-only ESO deployments cannot sync, and no render-time validation enforces the documented 'required' values | 2026-07-03 | `8b85de9` |
| 50 | GOMEMLIMIT is never set on ECS: the task definition's MEM_LIMIT env var is dead — the binary only reads the -container-mem-bytes flag, and the ECS container runs with no args | 2026-07-03 | `8b85de9` |
| 51 | Gray-tier key opted in via extra_indexed_fields or metadata_record_fields is accepted at config load but the guard denylist then drops every affected record (silent total data loss for the loop) | 2026-07-03 | `1ef0d83` |
| 52 | bucket_settle has no code default and 0 is silently accepted, breaking emit-once-after-settle; docs claim a 3m default that exists nowhere | 2026-07-03 | `96332f2` |
| 53 | Count the runs-loop max_backfill floor skip — a >24h outage's dropped log span is log-only, never alertable | 2026-07-03 | `4c75e89` |
| 54 | Resolve the /sessions query-shape contradiction: runs discovery omits sort_by=start_time that a sibling-loop comment declares required (bare offset 403s) on the real instance | 2026-07-03 | `4c75e89` |
| 55 | docs/langsmith.md sessions example config is invalid — `window: 1h` fails config validation at startup, and the session_filter example uses the wrong grammar | 2026-07-03 | `c67e422` |
| 56 | sessions and usage loops accept a stray non-zero LoopConfig.Window — flips the scheduler into windowed semantics and fires false backfill_unstorable alarms every tick | 2026-07-03 | `4c75e89` |
| 57 | Analytics loop silently no-ops forever when bootstrap_lookback <= bucket_settle or max_backfill is omitted/zero (raw-YAML configs) | 2026-07-03 | `4c75e89` |
| 58 | Signals()/telemetry docs declare wrong groups metric names (_by_ai_model/_by_metadata_value vs emitted _by_model/_by_metadata) and omit cost_usd_by_metadata | 2026-07-03 | `2b2ba3f` |
| 59 | CI e2e job executes an unpinned install script from k3d's main branch, and gitleaks/tool downloads are unverified and Renovate-blind | 2026-07-03 | `4872de5` |
| 60 | Instrument emit POST latency (the plain http.Client used for /v1/metrics + /v1/logs is in no histogram) | 2026-07-03 | `ae52bcf` `1c940e0` |
| 61 | Use HTTP Range requests to kill the per-chunk full-object re-download in logs_export (10× S3 egress on a full 50k page) | 2026-07-03 | `eae19e6` |
| 62 | Cross-host redirect guard permits same-host https->http scheme downgrade, leaking the vendor auth header in cleartext | 2026-07-03 | `eae19e6` |
| 63 | ValidateOwnership silently skips any loop that doesn't implement SeriesDeclarer, though emit-side dedup correctness depends on it as the only cross-loop collision gate | 2026-07-03 | `eae19e6` |
| 64 | Portkey analytics HTTP fixtures are encoded from the decode struct itself — JSON field-name regressions (tokens/latency/quota keys) cannot fail any test | 2026-07-03 | `9dea560` |
| 65 | Validate runs extra_record_fields/extra_indexed_fields against the known select enum — a typo'd opt-in 422s every runs/query at runtime (whole-loop outage) instead of failing fast | 2026-07-03 | `eae19e6` |
| 66 | Malformed JSONL export lines are dropped with only a Warn log — no self-metric, so a systematic format change drops 100% of records while the window completes cleanly | 2026-07-03 | `eae19e6` |
| 67 | CLAUDE.md/Chart.yaml still document scripts/publish.sh + 'make sbom' as the publish pipeline; the actual pipeline is the shared container-publish reusable | 2026-07-03 | `44c8c9e` |
| 68 | Release-please token documentation is stale: CLAUDE.md and the workflow's own header still claim GITHUB_TOKEN / no-CI-on-release-PR, but the workflow uses a PAT | 2026-07-03 | `c67e422` |
| 69 | Root VERSION file is orphaned at 2.1.1 (three releases stale) — release-please never updates it | 2026-07-03 | `19d0612` |
| 70 | followup.md's load-bearing protobuf v1.36.11 / client-go v0.35.6 pin decision was silently overridden by Renovate; go.mod now carries the exact pseudo-version the decision existed to avoid | 2026-07-03 | `19d0612` |
| 71 | Remove the stale .snyk suppression reference in ecs.go (Snyk was dropped from the fleet) | 2026-07-03 | `5ba48d7` |
| 72 | Set ReadHeaderTimeout on the health and pprof HTTP servers (Slowloris / G114) | 2026-07-03 | `5ba48d7` `609112c` |
| 73 | Graceful-drain claims (terminationGracePeriodSeconds=300 'lets an in-flight emit complete', DESIGN F23 drain, 'persist watermarks first on SIGTERM') do not match code — SIGTERM aborts in-flight work immediately | 2026-07-03 | `5ba48d7` |
| 74 | OnStoppedLeading logs 'leadership lost' on every clean shutdown of every replica, including never-elected standbys | 2026-07-03 | `a17b740` |
| 75 | Guard label allow-list unions BOTH vendors' keys unconditionally while the comment claims enabled-source-only union | 2026-07-03 | `1ef0d83` |
| 76 | ARCHITECTURE.md §11 and docs/DESIGN.md F1 list self-observability metric names that don't exist in the code and omit the real ones | 2026-07-03 | `5ba48d7` |
| 77 | Root CLAUDE.md Status line describes the ECS/DynamoDB feature as an open branch+PR, but it was squash-merged to main 5 days before the stated current date | 2026-07-03 | `c67e422` |
| 78 | docs/high-availability.md says CheckpointKey.SourceInstance comes from an 'id:' config field that doesn't exist | 2026-07-03 | `c67e422` |
| 79 | docs/troubleshooting.md and docs/alerts.md present '{kind="checkpoint_*"}' as a literal PromQL selector, which matches nothing | 2026-07-03 | `c67e422` |
| 80 | OTLP 200 partial-success responses (rejected_data_points / rejected_log_records) are silently discarded — spec-canonical partial rejects bypass the entire reject taxonomy with no counter | 2026-07-03 | `6ec9e1e` |
| 81 | Save does not validate that the watermark Time is encodable: an out-of-range year durably poisons the checkpoint key in both the configmap and dynamodb backends | 2026-07-03 | `44cd89a` |
| 82 | file checkpoint Store mutates in-memory state before flushing, so a failed durable write is masked as benign ErrStaleWrite on retry | 2026-07-03 | `44cd89a` |
| 83 | ARCHITECTURE.md #17 overclaims clock parity with K8s: DynamoDB failover compares absolute wall clocks across nodes, client-go does not | 2026-07-03 | `c67e422` |
| 84 | acquire() misreads its own committed-but-error-reported write as 'held by a live leader', locking the process out of its own lock for up to lease_duration | 2026-07-03 | `a17b740` |
| 85 | Doc/comment drift across deploy tree: chart version, readiness semantics, GOMEMLIMIT fraction, and dynamodb-table module version | 2026-07-03 | `8b85de9` |
| 86 | Setting networkPolicy.ingressPorts to empty inverts intent: it renders an allow-ALL-ingress rule instead of deny-all | 2026-07-03 | `8b85de9` |
| 87 | Empty leader-election identity is accepted silently: ECS metadata failure is swallowed, and on K8s lease two empty-identity replicas all become leader | 2026-07-03 | `a17b740` |
| 88 | Liveness threshold derivation includes DISABLED sources and loops, inflating the /healthz stale threshold beyond the documented 'slowest enabled cadence' | 2026-07-03 | `5ba48d7` |
| 89 | Shutdown/exit-path docs don't match code: lease leadership loss exits 0 (not the documented fatal), and tf/DESIGN claim in-flight emit + watermark save complete on SIGTERM when they are deliberately cancelled | 2026-07-03 | `c67e422` |
| 90 | Unset self-telemetry interval resolves to 60s/max_dpm instead of the documented 60s default — raising governance.max_dpm silently multiplies self-plane export rate | 2026-07-03 | `5ba48d7` |
| 91 | Version stamping is inert: internal/version is not linked into the binary, so the ldflags -X does nothing and no version is observable anywhere | 2026-07-03 | `5ba48d7` |
| 92 | Busy-skip path returns more=true for snapshot loops, so with max_catchup_per_tick>1 a snapshot loop (Window==0) accelerates to 2s ticks — contradicting the documented 'snapshot loops never accelerate' | 2026-07-03 | `f79c351` |
| 93 | Terminal-halt degrade is immediately cleared by the interior-bucket commit, so the scheduler does not back off on the tick that detected the terminal reject | 2026-07-03 | `f79c351` |
| 94 | backfill_unstorable re-counts the same abandoned span on every tick while collect/emit keeps failing, inflating samples_skipped_total | 2026-07-03 | `f79c351` |
| 95 | extra_record_fields can opt LLM-content previews (inputs_preview/outputs_preview) into emitted logs, contradicting the resolved "no LLM content to Grafana Cloud — ever" posture and followup.md's "rejected fail-fast at config load" claim | 2026-07-03 | `1ef0d83` |
| 96 | SSRF egress guard does not block the unspecified address (0.0.0.0 / ::), which dials to loopback services | 2026-07-03 | `44c8c9e` |
| 97 | AbsoluteNeverDenyKeys floor is exact-match while the repo's own docs claim gen_ai.* / gen_ai.prompt* prefix coverage — prefix variants bypass BOTH deny layers | 2026-07-03 | `1ef0d83` |
| 98 | Logs cardinality budget is keyed by bare loop name, so multiple source instances with the same-named loop share (and starve) one budget pool | 2026-07-03 | `f79c351` |
| 99 | labelSig does not escape '=' / ';' — distinct label sets collide, letting the per-series cardinality budget under-count | 2026-07-03 | `1ef0d83` |
| 100 | Package CLAUDE.md and docs/langsmith.md omit the usage loop entirely and carry stale defaults (max_pages_per_window 50 vs code 100) | 2026-07-03 | `c67e422` |
| 101 | runs session discovery maps 429 to a generic retryable error instead of source.ErrQuotaExceeded — quota misclassified in the self-metrics taxonomy | 2026-07-03 | `4c75e89` |
| 102 | session_label_value accepts any string silently — a typo ('Name', 'names') silently falls back to id, contradicting the settings contract stated in the same file | 2026-07-03 | `4c75e89` |
| 103 | Workspace-scope probe does not fire OnAuthError on 401/403 — with expected_workspace set, a credential failure never increments auth_errors_total | 2026-07-03 | `4c75e89` |
| 104 | deriveGroups can emit an undeclared cost_usd_by_prompt series if Portkey ever adds a cost field to prompt rows (SeriesNames/Key fingerprint blind spot) | 2026-07-03 | `4c75e89` |
| 105 | docs/portkey.md states bucket_settle default is 3 minutes and its example uses 3m — actual default is 10m and 3m was live-measured insufficient | 2026-07-03 | `2b2ba3f` |
| 106 | values.yaml/ECS comment for logs_export max_pages_per_window claims 'advances-past with a counted gap (never stalls)' — code errors loudly with NO advance (deliberate stall) | 2026-07-03 | `4c75e89` |
| 107 | Edge :main image + snapshot chart publish on every push to main regardless of CI results | 2026-07-03 | `54fd99f` |
| 108 | Forbidden-words gate scans only generic credential shapes on fork PRs — deployment-specific identifiers pass ci-success and are caught only after merge | 2026-07-03 | `f969458` `4872de5` |
| 109 | `make ci` omits tf-validate and `make gate` omits helm-lint, so the local 'full CI mirror' targets don't equal the CI matrix | 2026-07-03 | `54fd99f` |
| 110 | Lease coordinator exits the process (code 0) on leadership loss while the DynamoDB coordinator re-campaigns in-process — divergent, undocumented lifecycle | 2026-07-03 | `ae52bcf` `52a3f3f` |
| 111 | Resolved secret values can leak into fatal startup logs via YAML decode errors | 2026-07-03 | `ae52bcf` |
| 112 | ValidateConfigFile's placeholder heuristic false-FAILs valid configs (non-URL-named endpoint vars, env-parameterised numeric/duration fields) | 2026-07-03 | `ae52bcf` |
| 113 | emit.telemetry.otlp metric_interval is accepted by the schema but silently ignored | 2026-07-03 | `e1b99ae` |
| 114 | queue.max_batches / max_batch_bytes have no Load-time defaults — a non-Helm config omitting them silently runs with depth 1 and no proactive split | 2026-07-03 | `e1b99ae` |
| 115 | Doc-vs-code drift in the emit lane: CLAUDE.md claims single-sample 413 is treated as 'malformed', otlp.go claims Loki error shapes are still a follow-up (they are built), and DESIGN §4.5 claims a max-samples/records export bound that does not exist | 2026-07-03 | `54fd99f` |
| 116 | RMW retry budgets diverge between the configmap (6 attempts) and dynamodb (5 attempts) backends despite the dynamodb package doc claiming it mirrors the ConfigMap RMW exactly | 2026-07-03 | `52a3f3f` |
| 117 | configmap backend's corrupt-value refusal (CP-C10) has no test, despite the package CLAUDE.md claiming corruption is covered | 2026-07-03 | `9dea560` |
| 118 | internal/checkpoint/CLAUDE.md is stale: fence description omits the cursor relaxation and the Backends section omits the dynamodb backend entirely | 2026-07-03 | `54fd99f` |
| 119 | Alert on the loud-but-unalerted source_graph_unavailable_total graph values (export_failed / export_stuck / workspace_scope_mismatch) | 2026-07-03 | `61b626b` |
| 120 | Expose the degraded state machine as a gauge (genai_otel_bridge_loop_degraded{loop}) instead of leaving it inferable-only | 2026-07-03 | `ae52bcf` `1c940e0` |
| 121 | Extend upstream_request_duration_seconds buckets past 10s — LangSmith's client timeout is 30s and the docs say slow responses are expected | 2026-07-03 | `1c940e0` |
| 122 | Honor Retry-After on 429 in the emit retry loop (headers are currently discarded before backoff is computed) | 2026-07-03 | `52a3f3f` |
| 123 | go.mod now violates the recorded protobuf tagged-pin decision (untagged pseudo-version automerged by Renovate; the recorded revisit trigger has not occurred) | 2026-07-03 | `54fd99f` |
| 124 | .dockerignore misses local build artifacts (79 MB stray root binary, coverage.out, deploy/ecs/terraform/.terraform), bloating local build context and invalidating the COPY layer cache | 2026-07-03 | `61b626b` |
| 125 | ECS module does not inject GENAI_OTEL_BRIDGE_REPLICAS, so the all-leader double-emit guard has no defence-in-depth on ECS | 2026-07-03 | `61b626b` |
| 126 | No HEALTHCHECK in the Dockerfiles despite the binary shipping a -healthcheck probe mode | 2026-07-03 | `61b626b` |
| 127 | pprof port (pull-mode profiling) is opened to ALL cluster sources, not just the scraper | 2026-07-03 | `61b626b` |
| 128 | sourceEgressCIDR guidance ignores the Portkey logs_export signed-URL S3 download — tightening the CIDR per the values comment stalls the logs loop | 2026-07-03 | `61b626b` |
| 129 | Deferred shutdown funcs use unbounded context.Background() and the second SIGTERM/SIGINT is swallowed — a hung stop (e.g. active 300s pprof profile) blocks the final self-metrics flush until SIGKILL | 2026-07-03 | `1c940e0` |
| 130 | extra_record_fields on ANY enabled loop — including metrics loops that warn it away as an unknown setting — subtracts gray keys from the ONE shared guard denylist for every loop, silently weakening the defence-in-depth backstop | 2026-07-03 | `8e42dd8` |
| 131 | Docs claim a UUID-shape heuristic exists inside the guard; no such heuristic exists anywhere in internal/source | 2026-07-03 | `54fd99f` |
| 132 | Guard allow-list unions BOTH vendors' label keys regardless of source enablement, contradicting the adjacent 'ENABLED-source' comment and loosening the default-deny backstop *(not planned)* | 2026-07-03 |  |
| 133 | Registry.Register silently overwrites a duplicate type registration | 2026-07-03 | `eae19e6` |
| 134 | race_test.go exercises only the samples Sanitize path — SanitizeLogs/okLog shared-state concurrency is untested | 2026-07-03 | `9dea560` |
| 135 | source/CLAUDE.md's FROZEN interface snippet shows a stale Constructor signature (missing Deps) | 2026-07-03 | `54fd99f` |
| 136 | DESIGN §8 claims a golden-file OTLP protobuf determinism test that does not exist (determinism is comparative-only) | 2026-07-03 | `54fd99f` |
| 137 | Metrics-plane payload splitting (proactive MaxBytes + reactive 413 midpoint recursion) has no test; only the logs plane is covered | 2026-07-03 | `9dea560` |
| 138 | e2e zombie-leader test never resumes (SIGCONT) the frozen leader, so the real-cluster stale-writer fence path is unexercised | 2026-07-03 | `e9bd784` `9dea560` |
| 139 | Signed-URL validation checks host but not scheme — an http:// download URL to an allow-listed host is fetched over cleartext | 2026-07-03 | `eae19e6` |
| 140 | groups cross-page dedup by dimValue silently drops rows when distinct dimension values collapse to "" (non-string dim decode) | 2026-07-03 | `eae19e6` |
| 141 | stepIdle re-creates a fresh draft export job every tick while stalled on pages > max_pages_per_window (orphan-job spam + wasted rate budget) | 2026-07-03 | `eae19e6` |
| 142 | Production-readiness hardening — master tracker | 2026-07-03 | `f969458` |
| 149 | ci: release-please workflow fails at startup (startup_failure) since publish.yml gained wait-for-ci | 2026-07-03 | `ef6c318` |
| 150 | test(e2e): TestInvariant3 #138 fence-evidence assertion is unreachable, red on main | 2026-07-03 | `e9bd784` |
