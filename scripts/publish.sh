#!/usr/bin/env bash
# Publish the container image + Helm chart to an OCI registry.
#
# Single source of truth for CI and local publishing. Tagging is driven entirely by
# RELEASE_TAG:
#
#   RELEASE_TAG=v1.2.3   -> image :v1.2.3 + :latest, chart version 1.2.3
#   (unset/empty)        -> image :main  + :<shortsha>, chart 0.0.0-main.t<unixts>.<shortsha>
#
# The main-build chart version embeds a unix timestamp so the versions are time-SORTABLE, which lets
# ArgoCD track main with a semver-range targetRevision (e.g. '>=0.0.0-0 <0.0.1-0') and reliably resolve
# to the NEWEST main build. Two non-obvious SemVer details drove the exact format:
#   - The timestamp is prefixed with a literal 't' (t<unixts>) so the identifier is ALPHANUMERIC, not
#     pure-numeric. SemVer §11 ranks numeric prerelease identifiers BELOW alphanumeric ones, and the
#     legacy tags in the registry are 0.0.0-main.<sha> whose sha is alphanumeric (contains a-f). A
#     pure-numeric timestamp would therefore sort *below* every legacy sha tag and the range would pick
#     a stale build. 't' (0x74) sorts lexically above every hex digit (0-9/a-f), so t<ts> dominates all
#     legacy sha tags AND newer beats older (fixed 10-digit width => lexical order == chronological).
#   - A bare git sha alone sorts lexically (newest undefined); a moving 'main' tag is cached by ArgoCD's
#     repo-server and never re-pulled (cluster has timeout.hard.reconciliation: 0s). Hence neither works.
# The sha stays last purely for traceability / tie-break within the same second.
#
# Required env:
#   REGISTRY                           registry host (e.g. ghcr.io/your-org)
#   REGISTRY_USER, REGISTRY_PASSWORD   registry credentials (docker + helm login)
#   GIT_SHA                            commit sha (only needed for main builds)
#
# Optional env (defaults shown):
#   IMAGE_REPO=genai-otel-bridge                  image repository path under the registry
#   CHART_REPO=charts                  OCI namespace for the chart
#   PLATFORMS=linux/amd64,linux/arm64  buildx target platforms
#   HELM=helm                          helm binary (CI passes .tools/helm)
#   SETUP_BUILDX=1                     install qemu + create a buildx builder
set -euo pipefail

: "${REGISTRY:?REGISTRY is required (e.g. ghcr.io/your-org)}"
IMAGE_REPO="${IMAGE_REPO:-genai-otel-bridge}"
CHART_REPO="${CHART_REPO:-charts}"       # separate path from the image so the chart
                                         # and container don't collide in one OCI package
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
HELM="${HELM:-helm}"

: "${REGISTRY_USER:?REGISTRY_USER is required}"
: "${REGISTRY_PASSWORD:?REGISTRY_PASSWORD is required}"

IMAGE="${REGISTRY}/${IMAGE_REPO}"
# docker/helm login take a registry HOST (e.g. ghcr.io), not host/namespace (ghcr.io/owner). REGISTRY
# may carry a namespace path (GHCR), so strip it for login; the full REGISTRY is still used for tags/push.
REGISTRY_HOST="${REGISTRY%%/*}"

# ── resolve versions/tags from RELEASE_TAG ────────────────────────────────────
if [ -n "${RELEASE_TAG:-}" ]; then
  case "$RELEASE_TAG" in
    v*) ;;
    *) echo "RELEASE_TAG must look like v1.2.3 (got: $RELEASE_TAG)" >&2; exit 1 ;;
  esac
  app_version="$RELEASE_TAG"           # v1.2.3
  chart_version="${RELEASE_TAG#v}"     # 1.2.3 (helm wants bare semver)
  image_tags="${RELEASE_TAG} latest"
else
  short="$(printf '%s' "${GIT_SHA:?GIT_SHA is required for main builds}" | cut -c1-12)"
  ts="$(date +%s)"                       # unix seconds (10-digit, fixed width) — sortable, monotonic
  app_version="main-${short}"
  # t<ts> is ALPHANUMERIC on purpose (see header): lexically dominates legacy 0.0.0-main.<sha> tags and
  # sorts newest-first. <short> is the tie-break/traceability tail.
  chart_version="0.0.0-main.t${ts}.${short}"
  image_tags="main ${short}"
fi

echo "==> publishing image=${IMAGE} tags=[${image_tags}] app=${app_version} chart=${chart_version}"

# ── login (docker + helm OCI share the same registry creds) ───────────────────
printf '%s' "$REGISTRY_PASSWORD" | docker login "$REGISTRY_HOST" -u "$REGISTRY_USER" --password-stdin
printf '%s' "$REGISTRY_PASSWORD" | "$HELM" registry login "$REGISTRY_HOST" -u "$REGISTRY_USER" --password-stdin

# ── multi-arch buildx setup ───────────────────────────────────────────────────
if [ "${SETUP_BUILDX:-1}" = "1" ]; then
  docker run --privileged --rm tonistiigi/binfmt --install all >/dev/null 2>&1 || true
  # BUILDX_CREATE_ARGS lets a Docker-in-Docker-out runner give the buildkit
  # container host networking, so it can resolve a registry only known to the
  # host (e.g. a registry reachable only on the host network).
  docker buildx create --use --name genai-otel-bridge-builder ${BUILDX_CREATE_ARGS:-} >/dev/null 2>&1 \
    || docker buildx use genai-otel-bridge-builder
fi

# ── build + push image (all tags in one buildx invocation) ────────────────────
tag_flags=()
for t in $image_tags; do tag_flags+=(-t "${IMAGE}:${t}"); done

# --provenance/--sbom false: some OCI registries register each buildkit attestation
# manifest as a separate package "version", cluttering the package and hiding the
# real tags. We don't consume the attestations, so emit a clean index.
docker buildx build --platform "$PLATFORMS" \
  --provenance=false --sbom=false \
  --build-arg VERSION="$app_version" \
  "${tag_flags[@]}" \
  --push -f Dockerfile .

# ── package + push chart ──────────────────────────────────────────────────────
rm -rf dist && mkdir -p dist
"$HELM" package deploy/helm --version "$chart_version" --app-version "$app_version" -d dist/
"$HELM" push "dist/genai-otel-bridge-${chart_version}.tgz" "oci://${REGISTRY}/${CHART_REPO}"

echo "==> done: ${IMAGE} + oci://${REGISTRY}/${CHART_REPO}/genai-otel-bridge:${chart_version}"
