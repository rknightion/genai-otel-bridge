# internal/tenancy - the resolved plan

`Resolve(cfg)` turns a loaded config into a `Plan`: one `Row` per pipeline, holding the source
entry, its destination and its product identity. The plan is built once, before assembly, and
nothing consults it at emit time. Design: `docs/designs/multi-tenancy.md` section 3. The package
imports `internal/config` only.

## Shapes

- **Legacy shape** (`TenantShape` false): one row per enabled `sources` entry, in config order. The
  entry is copied into `Row.Source` unchanged, so `SourceInstance` (the checkpoint namespace) is the
  operator's own. Every row has `Destination = emit.telemetry.otlp` and `Identity =
  identity.ProductIdentity()` (a fresh map per row); tenant, target, pipeline, destination name and
  the two secret file names are empty. A disabled source gives no row. `Resolve` needs no validated
  config for this shape: it only copies enabled sources.
- **Legacy `mt.` warning.** `Resolve` logs a warning (slog's default logger) for each enabled legacy
  source whose `source_instance` starts with `mt.`, the prefix of tenant-shape checkpoint keys
  (design section 7): a later tenant and target with the same names would share its checkpoints. It is
  the one legacy-visible change of the tenant work here; the legacy rows, table and hash do not change.
- **Tenant shape** (`len(cfg.Tenants) > 0`): `Plan.TenantShape` is true and there is one row per
  target, sorted by tenant id then target name (so the table and hash do not depend on map order). A
  row has `Tenant`, `Target`, `Pipeline = PipelineID(tenant, target)` (`<tenant>.<target>`),
  `DestinationName`, the destination's endpoint and instance id (no token), the identity (the target's
  `identity` block whole, else the tenant's), and the two secrets as file names only (`AuthValueFile`,
  `TokenFile`): `Source.Auth.Value` and `Destination.Token` stay empty. `Row.Source` has `Enabled`
  true, the target's source block (type, base URL, auth header, rate limit, HTTP),
  `SourceInstance(tenant, target)` (`mt.<tenant>.<target>`), the target's `Scope`, and `Loops`
  deep-copied from `defaults.<type>.loops`: a new map, and in each block a new `settings` map and
  `graphs` slice, so no two rows (and no row and the config) share one. `TestRowsDoNotShareLoopMaps`
  guards this. `main` runs a tenant-shape plan and `app.ValidateConfigFile` assembles it (no secret
  file read) and returns its table and hash; nothing refuses the shape since unit 6e-2. A tenant with
  no targets gives no rows.
- **Resolve-phase rules** (design sections 2 and 3), checked by `Resolve` for the tenant shape. Every
  broken rule is reported (`errors.Join`, in tenant and target order) and the plan is then empty:
  - a target's `destination` names a destination of its own tenant;
  - no two tenants declare a destination on one stack (`StackKey`), over every declared destination;
  - no two targets of one tenant send to one stack with one identity;
  - no target sends to the self-telemetry stack (`emit.self`, else `emit.telemetry`, as `main` falls
    back) with the self-telemetry identity (the top-level identity with `config.SelfNamespaceSuffix`);
  - no scope name and no scope id in two tenants. `config.Validate` checks this at load too; `Resolve`
    does not call `Validate`, so it keeps its own copy. Keep both.

  The load rules are `config.Validate`'s; the assemble-phase rules (secrets, duplicate keys, scope
  assertion support, log-emitting loops) belong to `internal/app`.

`Row`, `Plan`, `Resolve`, `StackKey`, `PipelineID`, `SourceInstance`, `PipelineAttribute`,
`SecretReader` and `DirSecretReader` are seams later work depends on: keep their names and shapes. Add nothing to `Plan` beyond `Rows` and
`TenantShape` without a design change.

## Table and hash

- `Table()` is the plan as text: a header and one line per row with the columns of design section
  3. A legacy row shows its `source_instance` in the `pipeline` column. `scope_name` and `scope_id`
  are `Row.Source.Scope`; a row with no scope (every legacy row) shows `-` in both. Only enabled loops are
  listed, sorted; identity is `key=value` pairs, sorted by key.
- **It never prints a secret.** No `Auth.Value`, no `Token`, and every URL is printed without its
  userinfo, query and fragment (a URL that does not parse prints as `(unparseable)`).
  `TestTableNeverPrintsSecrets` guards this; extend it when a column is added.
- A tenant row's secrets are on the row as file names only (`AuthValueFile`, `TokenFile`), and the
  table has no column for them: the columns of design section 3 are the whole table, so a legacy plan's
  header and hash do not change. `TestResolveTenantOrderIndependent` pins a tenant table as a literal.
- An empty value prints as `-`; a value with white space, a control character, a quote or invalid
  UTF-8 is Go-quoted, so a row is always one line with one cell per column.
- `Hash()` is the lower-case hex SHA-256 of `Table()`. The layout is not a contract across versions:
  the hash is compared only between CI (`-validate-config`) and a leader on the same image. In the
  legacy shape an unset `${ENV}` ref is a placeholder in the config check, so the two hashes match
  only when CI has the same environment.

## SecretReader

`SecretReader.Read(tenant, file)` returns one tenant's secret file value. `app.BuildFromPlan` calls it
in assembly pass 1 for a row's `AuthValueFile` and `TokenFile` and puts the values on a copy of the
row, never on the plan. Its error names the tenant and the file and never carries a value.

`DirSecretReader(root)` (`secrets.go`) is the file-backed reader `main` uses, rooted at
`/var/run/secrets/genai-otel-bridge/tenants`: it reads `<root>/<tenant>/<file>` (symlinks followed, as
a Kubernetes Secret volume needs) and trims surrounding white space as the legacy `file:` resolver
does. It fails closed: a tenant or file that is not one bare path element (empty, `.`, `..`, a `/`,
a `\` or a NUL), a missing or unreadable file, a file over 64 KiB and a value empty after trimming are
all errors. Every error reads `tenant "<t>": secret file "<f>": <cause>` and carries neither a value
nor the path; a missing file still matches `fs.ErrNotExist`. `TestDirSecretReaderErrorsNameTenantAndFileOnly`
guards this.

## StackKey

`StackKey(endpoint, instanceID)` is the one definition of "the same stack": the endpoint with its host
(and scheme) lower-cased and trailing slashes dropped, joined to the instance id with `|`. The path
keeps its case. It holds the endpoint otherwise as written, so compare it; do not log it.
`app.BuildFromPlan` groups rows by it plus the identity map for the series-ownership check and the
guard.
