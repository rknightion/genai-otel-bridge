#!/usr/bin/env python3
"""Generate the decant self-observability dashboard as a gcx v2 Dashboard manifest.

Tracked generator (committed). Emits deploy/grafana/self-obs/dashboard-self-obs.yaml.
Run:  make gen-dashboard   (or)   python3 deploy/grafana/self-obs/gen_dashboard.py

v2 schema: dashboard.grafana.app/v2 with a TabsLayout (one tab per signal group) and
AutoGridLayout inside each tab (responsive — no hand-placed x/y). Per-loop staleness is
shown self-relative via the decant:freshness_ratio recording rule (>1 = staler than the
loop's own trailing-6h baseline) so a single colour scale serves every loop regardless of
its natural cadence. Multi-datasource: Prometheus (${datasource}), Loki (${loki}, the
poller's own stdout) and Pyroscope (${pyroscope}, the poller's own profiles).
"""
import json
import sys

PROM = "${datasource}"
LOKI = "${loki}"
PYRO = "${pyroscope}"

LOOP = '{loop=~"$loop"}'
RI = "$__rate_interval"
# The poller's OWN operational logs (Go slog -> stdout -> Alloy -> Loki). This is the
# integrator process, NOT the high-volume republished product logs (service_namespace=decant).
SELF_LOGS = '{namespace=~"$namespace", service_name="decant", container="decant"}'


# --------------------------------------------------------------------------- queries
def q(expr, legend="__auto", refid="A", instant=False, fmt="time_series"):
    return {"kind": "PanelQuery", "spec": {"hidden": False, "refId": refid, "query": {
        "kind": "DataQuery", "group": "prometheus", "version": "v0", "datasource": {"name": PROM},
        "spec": {"editorMode": "code", "expr": expr, "legendFormat": legend,
                 "range": not instant, "instant": instant, "exemplar": False, "format": fmt}}}}


def lq(expr, refid="A", queryType="range"):
    return {"kind": "PanelQuery", "spec": {"hidden": False, "refId": refid, "query": {
        "kind": "DataQuery", "group": "loki", "version": "v0", "datasource": {"name": LOKI},
        "spec": {"editorMode": "code", "expr": expr, "queryType": queryType, "maxLines": 1000}}}}


def pq(profile_type, refid="A", queryType="metrics", label_selector='{service_name="decant"}', group_by=None):
    return {"kind": "PanelQuery", "spec": {"hidden": False, "refId": refid, "query": {
        "kind": "DataQuery", "group": "grafana-pyroscope-datasource", "version": "v0", "datasource": {"name": PYRO},
        "spec": {"profileTypeId": profile_type, "labelSelector": label_selector,
                 "groupBy": group_by or [], "queryType": queryType}}}}


# --------------------------------------------------------------------------- panels
def _ts_fieldconfig(unit, fillop, thresholds=None):
    custom = {"drawStyle": "line", "lineInterpolation": "linear", "lineWidth": 1,
              "fillOpacity": fillop, "showPoints": "never", "spanNulls": False, "axisPlacement": "auto"}
    defaults = {"unit": unit, "custom": custom}
    if thresholds:
        custom["thresholdsStyle"] = {"mode": "line"}
        defaults["thresholds"] = {"mode": "absolute", "steps": thresholds}
        defaults["color"] = {"mode": "thresholds"}
    return {"defaults": defaults, "overrides": []}


def ts(pid, title, desc, unit, targets, *, fillop=10, placement="bottom", calcs=None, thresholds=None):
    calcs = calcs or ["lastNotNull", "max"]
    qs = [q(expr, legend, chr(ord("A") + i)) for i, (expr, legend) in enumerate(targets)]
    return {"kind": "Panel", "spec": {"id": pid, "title": title, "description": desc, "links": [],
        "data": {"kind": "QueryGroup", "spec": {"queries": qs, "queryOptions": {}, "transformations": []}},
        "vizConfig": {"kind": "VizConfig", "group": "timeseries", "spec": {
            "options": {"legend": {"displayMode": "table", "placement": placement, "showLegend": True, "calcs": calcs},
                        "tooltip": {"mode": "multi", "sort": "desc"}},
            "fieldConfig": _ts_fieldconfig(unit, fillop, thresholds)}}}}


def stat(pid, title, desc, expr, unit, *, color="value", graph="none", steps_=None, mappings=None, viz="stat"):
    fd = {"unit": unit, "color": {"mode": "thresholds"},
          "thresholds": {"mode": "absolute", "steps": steps_ or [{"color": "text", "value": None}]}}
    if mappings:
        fd["mappings"] = mappings
    opts = {"colorMode": color, "graphMode": graph, "justifyMode": "auto", "textMode": "auto",
            "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
            "wideLayout": True, "showPercentChange": False}
    if viz == "gauge":
        opts = {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                "showThresholdLabels": False, "showThresholdMarkers": True}
    return {"kind": "Panel", "spec": {"id": pid, "title": title, "description": desc, "links": [],
        "data": {"kind": "QueryGroup", "spec": {"queries": [q(expr, "", "A", instant=True)], "queryOptions": {}, "transformations": []}},
        "vizConfig": {"kind": "VizConfig", "group": viz, "spec": {"options": opts, "fieldConfig": {"defaults": fd, "overrides": []}}}}}


def logs_panel(pid, title, desc, expr):
    return {"kind": "Panel", "spec": {"id": pid, "title": title, "description": desc, "links": [],
        "data": {"kind": "QueryGroup", "spec": {"queries": [lq(expr)], "queryOptions": {}, "transformations": []}},
        "vizConfig": {"kind": "VizConfig", "group": "logs", "spec": {
            "options": {"showTime": True, "showLabels": False, "wrapLogMessage": True, "prettifyLogMessage": False,
                        "enableLogDetails": True, "dedupStrategy": "none", "sortOrder": "Descending"},
            "fieldConfig": {"defaults": {}, "overrides": []}}}}}


def loki_ts(pid, title, desc, unit, targets, *, placement="bottom"):
    qs = [lq(expr, chr(ord("A") + i)) for i, (expr, _legend) in enumerate(targets)]
    # legend comes from the LogQL aggregation labels
    return {"kind": "Panel", "spec": {"id": pid, "title": title, "description": desc, "links": [],
        "data": {"kind": "QueryGroup", "spec": {"queries": qs, "queryOptions": {}, "transformations": []}},
        "vizConfig": {"kind": "VizConfig", "group": "timeseries", "spec": {
            "options": {"legend": {"displayMode": "table", "placement": placement, "showLegend": True, "calcs": ["lastNotNull", "max"]},
                        "tooltip": {"mode": "multi", "sort": "desc"}},
            "fieldConfig": _ts_fieldconfig(unit, 30)}}}}


def pyro_ts(pid, title, desc, unit, profile_type):
    return {"kind": "Panel", "spec": {"id": pid, "title": title, "description": desc, "links": [],
        "data": {"kind": "QueryGroup", "spec": {"queries": [pq(profile_type, queryType="metrics")], "queryOptions": {}, "transformations": []}},
        "vizConfig": {"kind": "VizConfig", "group": "timeseries", "spec": {
            "options": {"legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
                        "tooltip": {"mode": "multi", "sort": "desc"}},
            "fieldConfig": _ts_fieldconfig(unit, 20)}}}}


def pyro_flame(pid, title, desc, profile_type):
    return {"kind": "Panel", "spec": {"id": pid, "title": title, "description": desc, "links": [],
        "data": {"kind": "QueryGroup", "spec": {"queries": [pq(profile_type, queryType="profile")], "queryOptions": {}, "transformations": []}},
        "vizConfig": {"kind": "VizConfig", "group": "flamegraph", "spec": {"options": {}, "fieldConfig": {"defaults": {}, "overrides": []}}}}}


# --------------------------------------------------------------------------- helpers
def steps(*pairs):
    return [{"color": c, "value": v} for v, c in pairs]


HEALTHY_MAP = [{"type": "value", "options": {"1": {"text": "HEALTHY", "color": "green", "index": 1}, "0": {"text": "STALE", "color": "red", "index": 0}}}]
PRESENT_MAP = [{"type": "value", "options": {"1": {"text": "PRESENT", "color": "green", "index": 0}}},
               {"type": "special", "options": {"match": "null", "result": {"text": "ABSENT", "color": "red", "index": 1}}}]
RATIO_STEPS = steps((None, "green"), (1.5, "yellow"), (2, "red"))
RATIO_LINES = steps((None, "green"), (1, "blue"), (2, "red"))

elements = {}


def add(p):
    elements[f"panel-{p['spec']['id']}"] = p


# === Tab: Overview / SLO =====================================================
add(stat(100, "Loops healthy", "HEALTHY when no selected loop is staler than 2x its OWN 6h baseline (max freshness_ratio < 2). Self-relative — won't read STALE just because a slow log-export loop is legitimately tens of minutes behind. Needs the recording rules pushed.",
         'max(decant:freshness_ratio{loop=~"$loop"}) < bool 2', "none", color="background", steps_=steps((None, "red"), (1, "green")), mappings=HEALTHY_MAP))
add(stat(101, "Leader present", "1 (PRESENT) if any selected loop exported a timestamp in the last 15m. ABSENT => leader gone / never emitted (distinct from stale). Inline — no recording rule required.",
         'max(present_over_time(decant_last_success_timestamp_seconds{loop=~"$loop"}[15m]))', "none", color="background", steps_=steps((None, "red"), (1, "green")), mappings=PRESENT_MAP))
add(stat(102, "Replicas emitting", "Distinct service_instance_id self-reporting. <2 => no standby (DecantNoStandby). Both active+standby self-emit, so 2 is the healthy HA steady state.",
         "count(group by (service_instance_id) (decant_window_lag_seconds))", "short", color="value", steps_=steps((None, "red"), (2, "green"))))
add(stat(103, "Worst freshness ratio", "max(decant:freshness_ratio): each loop's staleness over its OWN trailing-6h p90 baseline. <1.5 normal, >2 = much staler than this loop's normal. Uniform scale across loops of any cadence. Needs the recording rules pushed.",
         'max(decant:freshness_ratio{loop=~"$loop"})', "none", color="background", graph="area", steps_=RATIO_STEPS))
add(stat(104, "Max window lag", "now - watermark frontier, worst loop. Floors at ~bucket_settle+cadence by design; a sustained rise = a loop falling behind.",
         'max(decant_window_lag_seconds{loop=~"$loop"})', "s", color="value", graph="area", steps_=steps((None, "green"), (1800, "yellow"), (3600, "red"))))
add(stat(105, "Fatal emit errors (1h)", "sum(increase(decant_emit_errors_total{kind!='collect'}[1h])) — fatal kinds only (retryable_exhausted / checkpoint_* / bad_encoding); the benign upstream-fetch 'collect' retries are excluded. >0 => emit/commit is failing.",
         '(sum(increase(decant_emit_errors_total{loop=~"$loop",kind!="collect"}[1h])) or vector(0))', "short", color="background", steps_=steps((None, "green"), (1, "red"))))
add(ts(110, "Freshness ratio by loop (self-relative staleness)", "decant:freshness_ratio per loop. 1.0 = exactly the loop's own 6h-p90 staleness; the lines mark 1 (baseline) and 2 (alert). This is the decoupled, per-loop staleness view — a flat 15m threshold can't serve loops whose normal ranges from ~2m (sessions) to tens of minutes (runs).",
       "none", [('decant:freshness_ratio{loop=~"$loop"}', "{{loop}}")], thresholds=RATIO_LINES))
add(ts(111, "Emitted throughput by loop", "rate of metric samples (decant_emitted_total) and log records (decant_emitted_logs_total) successfully emitted, per loop. Zero across all while data exists upstream => emit broken.",
       "cps", [(f'sum by (loop) (rate(decant_emitted_total{LOOP}[{RI}]))', "{{loop}} samples"),
               (f'sum by (loop) (rate(decant_emitted_logs_total{LOOP}[{RI}]))', "{{loop}} logs")]))

# === Tab: Liveness & leadership ==============================================
add(ts(120, "Window lag by loop", "decant_window_lag_seconds per loop (now - watermark frontier). Flat-low is healthy; a sustained climb means the loop is not advancing its watermark.",
       "s", [('decant_window_lag_seconds{loop=~"$loop"}', "{{loop}}")]))
add(ts(121, "Last-success age vs each loop's baseline", "Solid = time() - last_success (age). Dashed = decant:last_success_age:baseline6h (the loop's own 6h-p90 normal). Age sawtoothing under its baseline is healthy; age pulling away above the baseline is the staleness signal DecantPollerStale fires on.",
       "s", [('time() - max by (loop) (decant_last_success_timestamp_seconds{loop=~"$loop"})', "{{loop}} age"),
             ('decant:last_success_age:baseline6h{loop=~"$loop"}', "{{loop}} baseline")]))
add(ts(122, "Replicas self-reporting over time", "Distinct service_instance_id. 2 = healthy active/passive HA; a drop to 1 = no standby (failover gap risk); a spike >2 sustained = leader overlap.",
       "short", [('count(group by (service_instance_id) (decant_window_lag_seconds))', "replicas")], calcs=["lastNotNull", "min"]))
add(stat(123, "Freshness ratio", "Per-loop staleness vs the loop's own 6h baseline (repeats once per selected loop). Green <1.5, red >2.",
         'decant:freshness_ratio{loop="$loop"}', "none", viz="gauge", steps_=RATIO_STEPS))

# === Tab: Emit pipeline ======================================================
add(ts(130, "Emitted samples /s by loop", "rate(decant_emitted_total). Metric data points successfully emitted.",
       "cps", [(f'sum by (loop) (rate(decant_emitted_total{LOOP}[{RI}]))', "{{loop}}")]))
add(ts(131, "Emitted logs /s by loop", "rate(decant_emitted_logs_total). Log records emitted (logs loops: logs_export, runs).",
       "cps", [(f'sum by (loop) (rate(decant_emitted_logs_total{LOOP}[{RI}]))', "{{loop}}")]))
add(ts(132, "Emit errors /s by loop & kind", "rate(decant_emit_errors_total) by kind. 'collect' = benign upstream-fetch retry; retryable_exhausted / checkpoint_* / bad_encoding are fatal (DecantEmitFailing).",
       "cps", [(f'sum by (loop, kind) (rate(decant_emit_errors_total{LOOP}[{RI}]))', "{{loop}} / {{kind}}")]))
add(ts(133, "Queue depth by loop", "decant_queue_depth_ratio (OTLP unit-1 gauge; '_ratio' is the OTLP->Prom suffix). Sustained non-zero => emit can't keep up with collect (backpressure → DecantQueueBackpressure).",
       "short", [(f'max by (loop) (decant_queue_depth_ratio{LOOP})', "{{loop}}")]))
add(ts(134, "Samples skipped /s (counted gaps) by reason", "rate(decant_samples_skipped_total) by reason. too_old / payload_too_large / duplicate_timestamp = real loss (DecantDataLoss); quota_exceeded / backfill_unstorable are benign. The loop advances past the bad bucket with a counted gap — never silent.",
       "cps", [(f'sum by (loop, reason) (rate(decant_samples_skipped_total{LOOP}[{RI}]))', "{{loop}} / {{reason}}")]))
add(ts(135, "Samples capped /s (DPM)", "rate(decant_samples_capped_total{reason=dpm}). Coalesced last-write-wins per series-minute to honour max_dpm. Expected non-zero when source resolution > cap — NOT data loss.",
       "cps", [(f'sum by (loop) (rate(decant_samples_capped_total{LOOP}[{RI}]))', "{{loop}}")]))
add(ts(136, "Guard dropped /s by loop (governance)", "rate(decant_guard_dropped_total). Data points / logs dropped by the cardinality/content governance Guard (default-deny label allow-list, field denylist). Expected non-zero if a denylist is configured.",
       "cps", [(f'sum by (loop) (rate(decant_guard_dropped_total{LOOP}[{RI}]))', "{{loop}}")]))
add(ts(137, "Buckets revised after settle /s", "rate(decant_bucket_revised_after_settle_total). A settled bucket changed value after bucket_settle (late arrivals). Persistent non-zero => widen the loop's bucket_settle (DecantBucketRevisedAfterSettle).",
       "cps", [(f'sum by (loop) (rate(decant_bucket_revised_after_settle_total{LOOP}[{RI}]))', "{{loop}}")]))
add(ts(138, "Bucket revision lateness (age p50/p95)", "histogram_quantile over decant_bucket_revised_after_settle_age_seconds — HOW LATE post-settle revisions are (now - bucketEnd), vs panel 137's how-often. Tune bucket_settle toward p95 to capture them (metrics can't be backfilled — Mimir rejects a changed value at an already-emitted timestamp). Floors at bucket_settle by construction.",
       "s", [(f'histogram_quantile(0.50, sum by (le, loop) (rate(decant_bucket_revised_after_settle_age_seconds_bucket{LOOP}[{RI}])))', "p50 {{loop}}"),
             (f'histogram_quantile(0.95, sum by (le, loop) (rate(decant_bucket_revised_after_settle_age_seconds_bucket{LOOP}[{RI}])))', "p95 {{loop}}")], fillop=0))

# === Tab: Upstream source health =============================================
add(ts(140, "Upstream request rate by target & status", "rate(decant_upstream_request_duration_seconds_count) by target (source API host) & status_class. The poller's own pull traffic to vendor APIs.",
       "reqps", [(f'sum by (target, status_class) (rate(decant_upstream_request_duration_seconds_count[{RI}]))', "{{target}} {{status_class}}")], calcs=["lastNotNull"]))
add(ts(141, "Upstream latency p50/p95/p99 by target", "histogram_quantile over decant_upstream_request_duration_seconds_bucket (cumulative histogram — rate() correct). Time-to-response-headers, excludes limiter wait.",
       "s", [(f'histogram_quantile(0.50, sum by (le, target) (rate(decant_upstream_request_duration_seconds_bucket[{RI}])))', "p50 {{target}}"),
             (f'histogram_quantile(0.95, sum by (le, target) (rate(decant_upstream_request_duration_seconds_bucket[{RI}])))', "p95 {{target}}"),
             (f'histogram_quantile(0.99, sum by (le, target) (rate(decant_upstream_request_duration_seconds_bucket[{RI}])))', "p99 {{target}}")], fillop=0))
add(ts(142, "Upstream error ratio by target", "decant:upstream_error_ratio:5m — fraction of requests to each target with status_class 4xx/5xx/error (incl. timeouts). >0.2 sustained => DecantUpstreamErrorBudget. Needs the recording rules pushed.",
       "percentunit", [('decant:upstream_error_ratio:5m', "{{target}}")], thresholds=steps((None, "green"), (0.2, "red"))))
add(ts(143, "Auth errors /s (401/403)", "rate(decant_auth_errors_total) by loop & source. Credential failure (wrong/expired key, missing scope) — distinct from slow/erroring endpoints. DecantAuthErrors fires on > 0.",
       "cps", [(f'sum by (loop, source) (rate(decant_auth_errors_total{LOOP}[{RI}]))', "{{loop}} / {{source}}")]))
add(ts(144, "Source graph unavailable /s (404 / window truncated)", "rate(decant_source_graph_unavailable_total) by graph. Steady increments on a real graph => API permanently absent (permission/capability) or timing out. graph=window_truncated => records dropped at the page cap (DecantWindowTruncatedDroppingRecords).",
       "cps", [(f'sum by (loop, graph) (rate(decant_source_graph_unavailable_total{LOOP}[{RI}]))', "{{loop}} / {{graph}}")]))

# === Tab: Cardinality & governance ===========================================
add(ts(150, "New label-value combinations /s by series (top 15)", "rate(decant_new_label_values_total) by series. Each is a never-before-seen label combo — a cardinality early-warning. A sustained climb on one series = unbounded labels (DecantCardinalitySpike).",
       "cps", [(f'topk(15, sum by (series) (rate(decant_new_label_values_total[{RI}])))', "{{series}}")], placement="right"))
add(ts(151, "Guard dropped /s by loop", "rate(decant_guard_dropped_total). Governance Guard drops (default-deny label allow-list / field denylist). The content-minimisation enforcement point.",
       "cps", [(f'sum by (loop) (rate(decant_guard_dropped_total{LOOP}[{RI}]))', "{{loop}}")]))
add(ts(152, "Samples capped /s (DPM)", "rate(decant_samples_capped_total). DPM cap coalescing — the cardinality/cost guard on the metrics plane.",
       "cps", [(f'sum by (loop) (rate(decant_samples_capped_total{LOOP}[{RI}]))', "{{loop}}")]))

# === Tab: Logs (the poller's own stdout) =====================================
add(loki_ts(160, "Log rate by level", "count_over_time of the poller's OWN stdout (namespace=$namespace, service_name=decant), by detected level. A warn/error climb corroborates the metric-side signals (e.g. Portkey fetch timeouts). Uses Loki's $__auto range (NOT $__rate_interval — that is Prometheus-only).",
            "cps", [(f'sum by (detected_level) (count_over_time({SELF_LOGS} [$__auto]))', "{{detected_level}}")]))
add(stat(162, "Warn+error logs (15m)", "Count of the poller's own warn/error stdout lines in the last 15m. Inline LogQL — independent of the metric pipeline, so it still reports if emit is down.", "0", unit="short"))
add(logs_panel(161, "Poller logs (warn+error)", "The integrator's own warn/error stdout. NOT the republished product logs (those are service_namespace=decant, high volume). Drill here when a metric signal fires.",
               f'{SELF_LOGS} | detected_level=~"warn|error"'))

# === Tab: Profiling (the poller's own runtime) ===============================
add(pyro_ts(170, "CPU usage (process_cpu)", "Continuous CPU profile of the integrator process (Pyroscope push). decant is near-idle between ticks; spikes align with poll/emit cycles.",
            "short", "process_cpu:cpu:nanoseconds:cpu:nanoseconds"))
add(pyro_ts(171, "Heap in-use", "Live heap (memory:inuse_space). Watch for a sustained climb (leak) vs the sawtooth of normal GC.",
            "bytes", "memory:inuse_space:bytes:space:bytes"))
add(pyro_ts(172, "Goroutines", "Goroutine count. A monotonic climb = a goroutine leak (e.g. a stuck HTTP call to a wedged upstream).",
            "short", "goroutines:goroutine:count:goroutine:count"))
add(pyro_flame(173, "CPU flame graph", "Where the integrator spends CPU. Use to confirm time is in poll/encode/emit and not somewhere unexpected.",
               "process_cpu:cpu:nanoseconds:cpu:nanoseconds"))


# --------------------------------------------------------------------------- layout
def gi(name):
    return {"kind": "AutoGridLayoutItem", "spec": {"element": {"kind": "ElementReference", "name": name}}}


def gi_repeat(name, var="loop"):
    return {"kind": "AutoGridLayoutItem", "spec": {"element": {"kind": "ElementReference", "name": name},
                                                   "repeat": {"mode": "variable", "value": var}}}


def auto_grid(items, *, cols=3, col_width="standard", row_height="standard"):
    return {"kind": "AutoGridLayout", "spec": {"maxColumnCount": cols, "columnWidthMode": col_width,
                                               "rowHeightMode": row_height, "fillScreen": False, "items": items}}


def tab(title, layout):
    return {"kind": "TabsLayoutTab", "spec": {"title": title, "layout": layout}}


tabs = [
    tab("Overview / SLO", auto_grid([
        gi("panel-100"), gi("panel-101"), gi("panel-102"), gi("panel-103"), gi("panel-104"), gi("panel-105"),
        gi("panel-110"), gi("panel-111"),
    ], cols=6, row_height="short")),
    tab("Liveness & leadership", auto_grid([
        gi("panel-120"), gi("panel-121"), gi("panel-122"), gi_repeat("panel-123"),
    ], cols=2)),
    tab("Emit pipeline", auto_grid([
        gi(f"panel-{i}") for i in range(130, 139)
    ], cols=3)),
    tab("Upstream source health", auto_grid([
        gi(f"panel-{i}") for i in range(140, 145)
    ], cols=2)),
    tab("Cardinality & governance", auto_grid([
        gi("panel-150"), gi("panel-151"), gi("panel-152"),
    ], cols=2)),
    tab("Logs", auto_grid([
        gi("panel-160"), gi("panel-162"), gi("panel-161"),
    ], cols=2)),
    tab("Profiling", auto_grid([
        gi("panel-170"), gi("panel-171"), gi("panel-172"), gi("panel-173"),
    ], cols=2)),
]

variables = [
    {"kind": "DatasourceVariable", "spec": {
        "name": "datasource", "label": "Prometheus", "pluginId": "prometheus",
        "current": {"text": "grafanacloud-prom", "value": "grafanacloud-prom"},
        "hide": "dontHide", "includeAll": False, "multi": False, "options": [], "regex": "",
        "refresh": "onDashboardLoad", "skipUrlSync": False, "allowCustomValue": True}},
    {"kind": "DatasourceVariable", "spec": {
        "name": "loki", "label": "Loki", "pluginId": "loki",
        "current": {"text": "grafanacloud-logs", "value": "grafanacloud-logs"},
        "hide": "dontHide", "includeAll": False, "multi": False, "options": [], "regex": "",
        "refresh": "onDashboardLoad", "skipUrlSync": False, "allowCustomValue": True}},
    {"kind": "DatasourceVariable", "spec": {
        "name": "pyroscope", "label": "Pyroscope", "pluginId": "grafana-pyroscope-datasource",
        "current": {"text": "grafanacloud-profiles", "value": "grafanacloud-profiles"},
        "hide": "dontHide", "includeAll": False, "multi": False, "options": [], "regex": "",
        "refresh": "onDashboardLoad", "skipUrlSync": False, "allowCustomValue": True}},
    {"kind": "QueryVariable", "spec": {
        "name": "loop", "label": "Loop", "definition": "label_values(decant_window_lag_seconds,loop)",
        "current": {"text": ["All"], "value": ["$__all"]}, "hide": "dontHide", "includeAll": True,
        "allValue": ".+", "multi": True, "options": [], "regex": "", "sort": "alphabeticalAsc",
        "refresh": "onDashboardLoad", "skipUrlSync": False, "allowCustomValue": True,
        "query": {"kind": "DataQuery", "group": "prometheus", "version": "v0", "datasource": {"name": PROM},
                  "spec": {"query": "label_values(decant_window_lag_seconds,loop)", "refId": "PrometheusVariableQueryEditor-VariableQuery"}}}},
    {"kind": "QueryVariable", "spec": {
        "name": "namespace", "label": "Namespace",
        "definition": "label_values({service_name=\"decant\"}, namespace)",
        "current": {"text": ["All"], "value": ["$__all"]}, "hide": "dontHide", "includeAll": True,
        "allValue": ".+", "multi": True, "options": [], "regex": "", "sort": "alphabeticalAsc",
        "refresh": "onDashboardLoad", "skipUrlSync": False, "allowCustomValue": True,
        "query": {"kind": "DataQuery", "group": "loki", "version": "v0", "datasource": {"name": LOKI},
                  "spec": {"query": "label_values({service_name=\"decant\"}, namespace)", "refId": "LokiVariableQueryEditor-VariableQuery"}}}},
]

spec = {
    "title": "decant — self-observability",
    "description": ("Operational health of the decant integrator itself (the decant_* self-metrics, the "
                    "poller's own stdout logs, and its runtime profiles). Tabbed top-down triage. Staleness "
                    "is shown self-relative (decant:freshness_ratio: >1 = staler than the loop's own 6h "
                    "baseline) so one scale serves loops of any cadence. Stack-agnostic: pick the Prometheus/"
                    "Loki/Pyroscope datasources via the variables. NOTE: emitted/last_success/window_lag are "
                    "recorded only on a SUCCESSFUL emit (the watermark must leave zero) — empty there is a "
                    "real signal that emit is failing, not a broken panel. The freshness/upstream-ratio panels "
                    "need the self-obs recording rules pushed."),
    "tags": ["decant", "self-obs", "gcx"],
    "editable": True, "liveNow": False, "preload": False, "cursorSync": "Crosshair", "revision": 1, "links": [],
    "annotations": [{"kind": "AnnotationQuery", "spec": {
        "builtIn": True, "enable": True, "hide": True, "iconColor": "rgba(0, 211, 255, 1)",
        "legacyOptions": {"type": "dashboard"}, "name": "Annotations & Alerts",
        "query": {"kind": "DataQuery", "group": "grafana", "version": "v0", "datasource": {"name": "-- Grafana --"}, "spec": {}}}}],
    "timeSettings": {"from": "now-6h", "to": "now", "timezone": "browser", "autoRefresh": "1m",
                     "autoRefreshIntervals": ["5s", "10s", "30s", "1m", "5m", "15m", "30m", "1h"],
                     "fiscalYearStartMonth": 0, "hideTimepicker": False},
    "variables": variables,
    "elements": elements,
    "layout": {"kind": "TabsLayout", "spec": {"tabs": tabs}},
}

manifest = {
    "apiVersion": "dashboard.grafana.app/v2", "kind": "Dashboard",
    "metadata": {"name": "decant-self-obs", "annotations": {"grafana.app/folder": "decant"},
                 "labels": {"grafana.app/folder": "decant"}},
    "spec": spec,
}

out = sys.argv[1] if len(sys.argv) > 1 else "deploy/grafana/self-obs/dashboard-self-obs.yaml"
with open(out, "w") as f:
    if out.endswith((".yaml", ".yml")):
        import yaml
        yaml.safe_dump(manifest, f, sort_keys=False, default_flow_style=False, width=4096)
    else:
        json.dump(manifest, f, indent=2)
        f.write("\n")
print(f"wrote {out} ({len(elements)} panels, {len(tabs)} tabs)")
