# Licensing

The default and only license for this repository is the **GNU Affero General Public License
v3.0 only** (`AGPL-3.0-only`). The full text is in [LICENSE](./LICENSE).

Every source file carries an SPDX identifier header:

```go
// SPDX-License-Identifier: AGPL-3.0-only
```

## Third-party dependencies

Vendored or module-cached third-party dependencies (e.g. under `vendor/` when present, or in the
Go module cache) remain under their own upstream licenses. Their licenses are not superseded by
the AGPL-3.0-only license of this repository; the combined binary is distributed under
AGPL-3.0-only while each dependency retains its original terms.

### Notices & SBOMs (release artifacts)

Third-party attribution is generated from the **actual import graph** of
`./cmd/genai-otel-bridge` (not from `go.mod`, which carries indirect/test-only deps that never
ship), using [`go-licenses`](https://github.com/google/go-licenses) and
[`syft`](https://github.com/anchore/syft):

- **`make notices`** → `THIRD_PARTY_NOTICES.md` — every linked module's `LICENSE` text, plus its
  `NOTICE` file where one exists (Apache-2.0 §4(d)). The container image bakes this into
  `/licenses/THIRD_PARTY_NOTICES.md` (alongside `/licenses/LICENSE`); the release pipeline also
  attaches it to each GitHub Release.
- **`make sbom`** → `dist/sbom/genai-otel-bridge.spdx.json` (SPDX 2.3) +
  `…cdx.json` (CycloneDX 1.6), attached to each GitHub Release.

These are **regenerated at release time, not committed** — they change on every dependency bump, so
committing and gating them would block hosted-Renovate automerge. They are therefore deliberately
**not** part of `make gate`. The image and the release assets always reflect exactly what shipped.

## Files derived from upstream code

Where a file is derived from third-party source, it additionally carries provenance headers
recording the origin and original license, for example:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/example/project/blob/main/path/file.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Example Authors.
```
