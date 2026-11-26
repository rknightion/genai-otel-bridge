#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Minute-resolution simulation of the GenaiOtelBridgePollerStale condition.

There is no alert-rule test harness in this repository, so this script is the proof for the
self-resolving-stall defect: it builds six hours of healthy sawtooth history for a loop, then a
permanent stall, and evaluates the rule condition once a minute for 24 hours.

  base       age > 2 * p90_6h(age) and age > 300
  candidate  (base) or age > CEILING

It also applies the rule's `for: 15m` to report when the alert fires and whether it ever resolves.
Keep the expressions in step with alertrule-poller-stale.yaml. Run: python3 sim_poller_stale.py

Scope gate (multi-tenant shape): the rule judges a pipeline only while its scope_state is matched, and keeps
a series with no tenant label judged as before. The gate cases model that: an unverified pipeline whose age
grows never fires, a matched one fires as today, a series with no tenant is judged as today. They prove the
gate's arithmetic; TestBundledRuleGroupings pins the PromQL join. `--ungated` evaluates them against the
condition without the gate (the red run: the unverified pipeline fires, so it prints FAIL).
"""
import sys

CEILING = 10800  # seconds; the absolute arm, above the slowest healthy sawtooth (see the rule comment)
FLOOR = 300
FOR_MIN = 15
HISTORY_MIN = 360
STALL_MIN = 24 * 60


def p90(values):
    """PromQL quantile_over_time(0.9, ...) with linear interpolation between ranks."""
    s = sorted(values)
    rank = 0.9 * (len(s) - 1)
    lo = int(rank)
    hi = min(lo + 1, len(s) - 1)
    return s[lo] + (s[hi] - s[lo]) * (rank - lo)


def condition(age, window, with_ceiling):
    rel = age > 2 * p90(window) and age > FLOOR
    return rel or (with_ceiling and age > CEILING)


def judged(has_tenant, matched, gate):
    """The scope gate: a tenant series counts only when matched; a series with no tenant always counts."""
    return (not gate) or (not has_tenant) or matched


def simulate(healthy_max_min, with_ceiling, has_tenant=False, matched=True, gate=True):
    # Healthy sawtooth: age climbs 0..healthy_max_min minutes then resets at a successful poll.
    period = healthy_max_min + 1
    ages = [float((m % period) * 60) for m in range(HISTORY_MIN)]
    age0 = ages[-1]
    truth = []
    for k in range(1, STALL_MIN + 1):
        ages.append(age0 + k * 60)
        window = ages[-HISTORY_MIN:]
        truth.append(judged(has_tenant, matched, gate) and condition(ages[-1], window, with_ceiling))
    fired_at = None
    run = 0
    for i, t in enumerate(truth, start=1):
        run = run + 1 if t else 0
        if run >= FOR_MIN and fired_at is None:
            fired_at = i
    first_true = next((i for i, t in enumerate(truth, start=1) if t), None)
    last_false = max((i for i, t in enumerate(truth, start=1) if not t), default=0)
    return {
        "first_true_min": first_true,
        "fires_at_min": fired_at,
        "true_at_end": truth[-1],
        "true_to_end_from_min": last_false + 1 if truth[-1] else None,
        "resolves_while_stalled": fired_at is not None and not truth[-1],
    }


def gate_cases(gate):
    """Per-case (name, has_tenant, matched, want_fire). Every case stalls permanently after healthy history."""
    ok = True
    print("scope gate" if gate else "scope gate (ungated: the red run)")
    for name, has_tenant, matched, want_fire in (
        ("unverified pipeline never fires", True, False, False),
        ("matched pipeline fires as today", True, True, True),
        ("series with no tenant is judged as today", False, False, True),
    ):
        r = simulate(10, True, has_tenant=has_tenant, matched=matched, gate=gate)
        fired = r["fires_at_min"] is not None
        print(f"  {name}: fires={fired} want={want_fire}")
        ok &= fired == want_fire
    return ok


def main():
    ok = True
    if "--ungated" in sys.argv:
        red_ok = gate_cases(False)
        print("PASS" if red_ok else "FAIL")
        return 0 if red_ok else 1
    for label, with_ceiling in (("base", False), ("candidate", True)):
        print(label)
        for h in (2, 10, 30, 50):
            r = simulate(h, with_ceiling)
            print(f"  healthy age 0-{h:>2}m: {r}")
            if with_ceiling:
                # AC: once past the ceiling the condition stays true to the end of the 24h stall.
                ceiling_min = CEILING // 60
                ok &= r["true_at_end"] and (r["true_to_end_from_min"] or 10**9) <= ceiling_min + h + 1
                ok &= r["fires_at_min"] is not None and not r["resolves_while_stalled"]
    ok &= gate_cases(True)
    print("PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
