#!/usr/bin/env bash
# k3d 3-node failover e2e: create cluster, import images, helm-install the chart with the e2e overlay,
# then run the Go failover suite (tag e2e). A second phase upgrades the same release to the tenant shape
# and runs the tenant tests. `all` also checks uninstall and config revisions before teardown.
set -euo pipefail
CLUSTER="${E2E_CLUSTER:-genai-otel-bridge-e2e}"
NS="${E2E_NAMESPACE:-genai-otel-bridge-e2e}"
IMAGE="${IMAGE:-genai-otel-bridge:dev}"
E2E_HELPER_IMAGE="${E2E_HELPER_IMAGE:-genai-otel-bridge-e2e-helper:dev}"
# No fallback tag: the pin lives only in the justfile (k3s_image), where Renovate can move it.
# Only up() needs it, so down and verify-cleanup work without it.
K3S_IMAGE="${K3S_IMAGE:-}"
# Exported so the Go suite's kubectl-debug freeze uses the same (imported) utility image.
export BUSYBOX_IMAGE="${BUSYBOX_IMAGE:-busybox:1.36}"
TOOLS_DIR="${TOOLS_DIR:-$PWD/.tools}"
export PATH="$TOOLS_DIR:$PATH"

up() {
  [ -n "$K3S_IMAGE" ] || { echo "K3S_IMAGE is not set: run via 'just e2e' or 'just k3d-up', or export K3S_IMAGE=rancher/k3s:<tag>" >&2; exit 1; }
  # Idempotent: clear any cluster left over from an interrupted/killed prior run
  # (e.g. a superseded CI run) so `cluster create` doesn't fail with "already exists".
  k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
  local create_extra=()
  if [ -n "${E2E_DOOD:-}" ]; then
    # Docker-out-of-Docker CI: this script runs inside a job container, but the
    # docker daemon — and therefore the k3d node containers — live on the host.
    # Publish the k3s API on the docker bridge gateway (this container's route to
    # the host) so k3d's own --wait, plus kubectl/helm below, can reach it. k3d
    # derives the kubeconfig server address and the API cert SAN from --api-port.
    # Ask the daemon for the bridge gateway (|| true so set -e doesn't abort here).
    local gw; gw="$(docker network inspect bridge --format '{{ (index .IPAM.Config 0).Gateway }}' 2>/dev/null || true)"
    [ -n "$gw" ] || { echo "E2E_DOOD set but could not determine the docker bridge gateway" >&2; exit 1; }
    local port="${E2E_API_PORT:-6445}"
    echo "E2E_DOOD: publishing k3s API on host bridge gateway ${gw}:${port}"
    create_extra+=(--api-port "${gw}:${port}")
  fi
  # ${arr[@]+"${arr[@]}"} guards the empty-array expansion: stock macOS bash 3.2 treats "${arr[@]}"
  # of an empty array as an unbound variable under `set -u` and aborts. No-op on bash 5 (CI).
  k3d cluster create "$CLUSTER" --servers 1 --agents 2 --image "$K3S_IMAGE" ${create_extra[@]+"${create_extra[@]}"} --wait --timeout 120s
  # busybox (inv-3 zombie-freeze ephemeral container) is multi-arch. A containerd-backed
  # Docker store can save its OCI index without the other-platform blobs, so the combined
  # k3d import fails before the app can run. Hosted Docker can use this store too; DooD
  # placement does not identify the image format. Flatten the utility image for both paths.
  docker build -q -t "$BUSYBOX_IMAGE" - >/dev/null <<EOF
FROM $BUSYBOX_IMAGE
EOF
  # 3-node import (simple + robust for a few small images; revisit a --registry-create if it gets slow).
  k3d image import "$IMAGE" "$E2E_HELPER_IMAGE" "$BUSYBOX_IMAGE" -c "$CLUSTER"
  export KUBECONFIG; KUBECONFIG="$(k3d kubeconfig write "$CLUSTER")"
  kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
  # The manifests carry no namespace: -n puts them in $NS, so E2E_NAMESPACE moves them with the release.
  # Only the legacy phase's Secret here: the tenant Secrets are applied by run_tenant_tests.
  kubectl apply -n "$NS" -f test/e2e/manifests/secrets.yaml
  # Install the images this run built and imported, so IMAGE and E2E_HELPER_IMAGE override the
  # defaults in values-e2e.yaml (extraContainers[0] is the OTLP sink, [1] the mock Portkey).
  helm install genai-otel-bridge deploy/helm -n "$NS" -f test/e2e/values-e2e.yaml \
    --set image="$IMAGE" \
    --set "extraContainers[0].image=$E2E_HELPER_IMAGE" \
    --set "extraContainers[1].image=$E2E_HELPER_IMAGE" \
    --wait --timeout 180s
  kubectl -n "$NS" rollout status deploy/genai-otel-bridge --timeout 120s
}

down() { k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true; }

run_tests() {
  export KUBECONFIG; KUBECONFIG="$(k3d kubeconfig write "$CLUSTER")"
  export E2E_NAMESPACE="$NS"
  export KUBECTL="$TOOLS_DIR/kubectl"
  # The tenant tests need the tenant-shape release: run_tenant_tests runs them after the upgrade.
  go test -tags e2e -count=1 -timeout 15m -v -skip '^TestTenant' ./test/e2e/...
}

# The tenant phase: upgrade the same release (the chart is one fixed-name release, so it keeps the Lease
# and the checkpoint ConfigMap) to the tenant shape of values-e2e-tenants.yaml, layered on the legacy
# overlay, then run only the tenant tests. Run after run_tests: the legacy release with this image and
# then the tenant-shape config is the two-step conversion of the multi-tenancy design (section 10).
run_tenant_tests() {
  export KUBECONFIG; KUBECONFIG="$(k3d kubeconfig write "$CLUSTER")"
  export E2E_NAMESPACE="$NS"
  export KUBECTL="$TOOLS_DIR/kubectl"
  kubectl apply -n "$NS" -f test/e2e/manifests/tenant-secrets.yaml
  helm upgrade genai-otel-bridge deploy/helm -n "$NS" -f test/e2e/values-e2e.yaml -f test/e2e/values-e2e-tenants.yaml \
    --set image="$IMAGE" \
    --set "extraContainers[0].image=$E2E_HELPER_IMAGE" \
    --set "extraContainers[1].image=$E2E_HELPER_IMAGE" \
    --wait --timeout 180s
  kubectl -n "$NS" rollout status deploy/genai-otel-bridge --timeout 120s
  go test -tags e2e -count=1 -timeout 15m -v -run '^TestTenant' ./test/e2e/...
}

# Exercise the post-delete cleanup hook: a real `helm uninstall` (NOT the cluster-delete teardown,
# which bypasses helm) must leave no orphaned app-created HA objects (lease + checkpoint ConfigMap).
# Run AFTER run_tests so it doesn't disturb the failover suite. By then the leader-election lease and
# the checkpoint ConfigMap must both exist: if either is absent its half of the cleanup check would
# pass vacuously, so that is a failure.
uninstall_verify() {
  export KUBECONFIG; KUBECONFIG="$(k3d kubeconfig write "$CLUSTER")"
  kubectl -n "$NS" get lease genai-otel-bridge-leader >/dev/null 2>&1 \
    || { echo "PRE-FAIL: lease genai-otel-bridge-leader absent before uninstall (cleanup test would be vacuous)" >&2; exit 1; }
  kubectl -n "$NS" get configmap genai-otel-bridge-checkpoints >/dev/null 2>&1 \
    || { echo "PRE-FAIL: configmap genai-otel-bridge-checkpoints absent before uninstall (cleanup test would be vacuous)" >&2; exit 1; }
  echo "pre-uninstall: lease and checkpoint ConfigMap present"
  # --wait blocks on the post-delete hook Job (genai-otel-bridge -cleanup) completing.
  helm uninstall genai-otel-bridge -n "$NS" --wait --timeout 120s
  local fail=0
  if kubectl -n "$NS" get lease genai-otel-bridge-leader >/dev/null 2>&1; then
    echo "FAIL: lease genai-otel-bridge-leader orphaned after helm uninstall" >&2; fail=1
  fi
  if kubectl -n "$NS" get configmap genai-otel-bridge-checkpoints >/dev/null 2>&1; then
    echo "FAIL: configmap genai-otel-bridge-checkpoints orphaned after helm uninstall" >&2; fail=1
  fi
  [ "$fail" -eq 0 ] || exit 1
  echo "uninstall cleanup OK: post-delete hook removed the lease + checkpoint ConfigMap"
}

case "${1:-all}" in
  up) up ;;
  down) down ;;
  verify-cleanup) uninstall_verify ;;
  tenant-tests) run_tenant_tests ;;
  all)
    trap down EXIT
    up; run_tests; run_tenant_tests; uninstall_verify
    bash test/helm/config-revisions-e2e.sh "$NS-revisions" "$IMAGE" "$E2E_HELPER_IMAGE" "$BUSYBOX_IMAGE"
    ;;
  *) echo "usage: $0 up|down|verify-cleanup|tenant-tests|all"; exit 2 ;;
esac
