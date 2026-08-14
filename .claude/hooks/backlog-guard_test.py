#!/usr/bin/env python3
"""Tests for backlog-guard.py.

Negative-tested on purpose: the safe forms MUST exit 0, not merely the unsafe
forms exit 2. A guard that blocks everything passes a deny-only test suite and
makes the tracker unusable.

Paths are derived from this file's own location so the suite runs from any
checkout on any machine.

Run: python3 .claude/hooks/backlog-guard_test.py

Note the `"--" + "notes"` splitting below. The guard denies any Bash command
containing both `backlog` and a bare section flag — so if you ever run these
cases via a shell one-liner instead of this file, the command string itself
trips the hook. Keeping the literal out of the source is the cheap defence.
"""

import json
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
HOOK = os.path.join(HERE, "backlog-guard.py")
ROOT = os.path.dirname(os.path.dirname(HERE))
env = dict(os.environ, CLAUDE_PROJECT_DIR=ROOT)

N = "--" + "notes"
P = "--" + "plan"

cases = [
    # DENY — the two silent-destruction footguns
    ("bare notes flag",      {"tool_name": "Bash", "tool_input": {"command": f"backlog task edit gob-0001 {N} hi"}}, 2),
    ("bare plan flag",       {"tool_name": "Bash", "tool_input": {"command": f"backlog task edit gob-0001 {P} hi"}}, 2),
    ("equals form",          {"tool_name": "Bash", "tool_input": {"command": f"backlog task edit gob-0001 {N}=hi"}}, 2),
    ("flag at end of line",  {"tool_name": "Bash", "tool_input": {"command": f"backlog task edit gob-0001 {N}"}}, 2),
    ("edit task md",         {"tool_name": "Edit",  "tool_input": {"file_path": f"{ROOT}/backlog/tasks/gob-0001 - x.md"}}, 2),
    ("write doc md",         {"tool_name": "Write", "tool_input": {"file_path": f"{ROOT}/backlog/docs/doc-0002 - y.md"}}, 2),
    ("edit completed md",    {"tool_name": "Edit",  "tool_input": {"file_path": f"{ROOT}/backlog/completed/gob-0009 - z.md"}}, 2),
    ("edit archived task",   {"tool_name": "Edit",  "tool_input": {"file_path": f"{ROOT}/backlog/archive/tasks/gob-0003 - a.md"}}, 2),

    # ALLOW — the safe forms. These matter more than the denies.
    ("append-notes allowed", {"tool_name": "Bash", "tool_input": {"command": "backlog task edit gob-0001 --append-notes hi"}}, 0),
    ("append-plan allowed",  {"tool_name": "Bash", "tool_input": {"command": "backlog task edit gob-0001 --append-plan hi"}}, 0),
    ("task list allowed",    {"tool_name": "Bash", "tool_input": {"command": "backlog task list --plain"}}, 0),
    ("task create allowed",  {"tool_name": "Bash", "tool_input": {"command": "backlog task create 'x' -d 'y' --ac 'z'"}}, 0),
    ("finalize allowed",     {"tool_name": "Bash", "tool_input": {"command": "backlog task edit gob-0007 --check-ac 1 -s Done"}}, 0),
    ("doc update allowed",   {"tool_name": "Bash", "tool_input": {"command": "backlog doc update doc-0002 --content x"}}, 0),
    ("non-backlog cmd",      {"tool_name": "Bash", "tool_input": {"command": f"mytool {N} foo"}}, 0),
    ("config.yml allowed",   {"tool_name": "Edit",  "tool_input": {"file_path": f"{ROOT}/backlog/config.yml"}}, 0),
    ("source file allowed",  {"tool_name": "Edit",  "tool_input": {"file_path": f"{ROOT}/internal/app/app.go"}}, 0),
    ("AGENTS.md allowed",    {"tool_name": "Write", "tool_input": {"file_path": f"{ROOT}/AGENTS.md"}}, 0),
    ("archive json allowed", {"tool_name": "Write", "tool_input": {"file_path": f"{ROOT}/archive/github-issues-2026-08-14.json"}}, 0),
]

fails = 0
for name, payload, want in cases:
    r = subprocess.run([sys.executable, HOOK], input=json.dumps(payload),
                       capture_output=True, text=True, env=env)
    ok = r.returncode == want
    fails += not ok
    print(f"{'PASS' if ok else 'FAIL'}  exit={r.returncode} want={want}  {name}")

# Garbage stdin must never block: a guard that fails closed on an unparseable
# payload takes the whole session down.
r = subprocess.run([sys.executable, HOOK], input="not json", capture_output=True, text=True, env=env)
ok = r.returncode == 0
fails += not ok
print(f"{'PASS' if ok else 'FAIL'}  exit={r.returncode} want=0  garbage stdin never blocks")

total = len(cases) + 1
print(f"\n{total - fails}/{total} passed")
sys.exit(1 if fails else 0)
