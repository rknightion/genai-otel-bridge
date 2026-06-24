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

## Files derived from upstream code

Where a file is derived from third-party source, it additionally carries provenance headers
recording the origin and original license, for example:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/example/project/blob/main/path/file.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Example Authors.
```
