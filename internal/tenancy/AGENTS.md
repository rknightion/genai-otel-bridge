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
- **Tenant shape**: not resolved yet. Its rows carry `PipelineID(tenant, target)` (`<tenant>.<target>`),
  `SourceInstance(tenant, target)` (`mt.<tenant>.<target>`) and the secrets as file names
  (`AuthValueFile`, `TokenFile`), never values.

`Row`, `Plan`, `Resolve`, `StackKey`, `PipelineID`, `SourceInstance`, `PipelineAttribute` and
`SecretReader` are seams later work depends on: keep their names and shapes. Add nothing to `Plan` beyond `Rows` and
`TenantShape` without a design change.

## Table and hash

- `Table()` is the plan as text: a header and one line per row with the columns of design section
  3. A legacy row shows its `source_instance` in the `pipeline` column. `scope_name` and `scope_id`
  are `Row.Source.Scope`; a row with no scope (every legacy row) shows `-` in both. Only enabled loops are
  listed, sorted; identity is `key=value` pairs, sorted by key.
- **It never prints a secret.** No `Auth.Value`, no `Token`, and every URL is printed without its
  userinfo, query and fragment (a URL that does not parse prints as `(unparseable)`).
  `TestTableNeverPrintsSecrets` guards this; extend it when a column is added.
- An empty value prints as `-`; a value with white space, a control character, a quote or invalid
  UTF-8 is Go-quoted, so a row is always one line with one cell per column.
- `Hash()` is the lower-case hex SHA-256 of `Table()`. The layout is not a contract across versions:
  the hash is compared only between CI (`-validate-config`) and a leader on the same image. In the
  legacy shape an unset `${ENV}` ref is a placeholder in the config check, so the two hashes match
  only when CI has the same environment.

## SecretReader

`SecretReader.Read(tenant, file)` returns one tenant's secret file value. `app.BuildFromPlan` calls it
in assembly pass 1 for a row's `AuthValueFile` and `TokenFile` and puts the values on a copy of the
row, never on the plan. Its error names the tenant and the file and never carries a value. This
package defines only the interface; the file-backed reader comes later.

## StackKey

`StackKey(endpoint, instanceID)` is the one definition of "the same stack": the endpoint with its host
(and scheme) lower-cased and trailing slashes dropped, joined to the instance id with `|`. The path
keeps its case. It holds the endpoint otherwise as written, so compare it; do not log it.
`app.BuildFromPlan` groups rows by it plus the identity map for the series-ownership check and the
guard.
