# Archive

## `github-issues-2026-08-14.json`

**The complete record of this repo's GitHub Issues, captured on 2026-08-14 when the project moved
to Backlog.md.** The closed issues were deleted from GitHub on that date, so `gh issue view <N>`
404s for them — this file is the record, not a convenience copy. The Backlog doc *"Closed GitHub
issues (pre-Backlog history index)"* is the index into it.

Read a single issue:

```sh
jq '.[] | select(.number == 142)' archive/github-issues-2026-08-14.json
```

Find every issue whose body mentions a thing:

```sh
jq -r '.[] | select((.body // "") | test("checkpoint"; "i")) | "#\(.number) \(.title)"' \
  archive/github-issues-2026-08-14.json
```

### What it contains

119 issues (2 open at capture time, 117 closed), sorted by number, with `number`, `title`, `body`,
`comments`, `labels`, `state`, `stateReason`, `author`, `createdAt`, `updatedAt`, `closedAt`, `url`,
`milestone` and `assignees`.

**Comment completeness was verified, not assumed.** `gh issue list --json comments` paginates, so
the dump's per-issue comment counts were summed and required to match the REST API's own
`.comments` field across `repos/rknightion/genai-otel-bridge/issues?state=all` with `--paginate`:
**11 = 11 across 119 issues**. The two counts agreeing is the check; a merely non-empty `comments`
array is not.

The **open** issues are archived too, deliberately. `#165` survives as a Backlog task and `#2` is
Renovate's dependency dashboard, which stays on GitHub — neither was deleted. Archiving everything
is what lets the delete list be asserted as a subset of the archive rather than argued about.

### Redaction: none was required, and that is a finding, not an omission

Everything in this file is unredacted, because a per-field sweep found nothing to redact.

The sweep ran over **1,934 decoded string fields** — every `title`, `body`, comment body, author
login, label name and description, milestone, assignee and URL — **not** over the serialized JSON.
That distinction is the whole point: in `json.dumps` output an escape such as `\n` leaves a literal
`n` immediately before the following word, which breaks a `\b` word boundary, so a sweep of the
serialized blob can certify a file clean while it still leaks.

Hunted for: email addresses; Grafana Cloud stack hosts, stack IDs and per-database tenant IDs;
Grafana prod cluster hostnames; AWS account IDs, ARNs and access key IDs; UUIDs; Portkey,
LangSmith, Grafana Cloud and GitHub credential shapes; private key headers; `Authorization`/bearer
values; public IPv4 literals; internal/corp/local hostnames; and known personal host and
organisation names.

Three benign hits, each already present in the tracked public tree, none redacted:

| Hit | Where | Why it stays |
|---|---|---|
| `m7kni` | `#26`, `#42` | The public docs hub `m7kni.io` this repo's site is served under. Already in `README.md`, `docs.toml`, `deploy/helm/Chart.yaml`, `.github/workflows/trigger-docs-sync.yml`. |
| `opnsense` | `#165` | The sibling public repo `rknightion/opnsense-exporter`, cited as cross-repo precedent. |
| `forgejo` | `#124` | A `.dockerignore` entry quoted verbatim in an issue about build-context bloat. Already in `.dockerignore` and `renovate.json`. |

Long hex strings appear only inside `#2`, Renovate's dependency dashboard: they are git commit SHAs
and container image digests, which are public by construction.

**If you re-run this for another repo, do not copy the conclusion — copy the method.** A clean
result here reflects that these issues were adversarial code reviews of a decoupled OSS service,
whose own `scripts/forbidden-words.sh` gate already keeps deployment identifiers out of the tree.
That gate scans `archive/` too, so CI re-checks this file against the real (secret-injected)
identifier list on every run.
