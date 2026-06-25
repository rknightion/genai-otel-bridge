#!/usr/bin/env bash
# k3d 3-node failover e2e: create cluster, import images, helm-install the chart with the e2e overlay,
# then run the Go failover suite (tag e2e). `all` brings it up, tests, and tears down.
set -euo pipefail
CLUSTER="${E2E_CLUSTER:-decant-e2e}"
NS="${E2E_NAMESPACE:-decant-e2e}"
IMAGE="${IMAGE:-decant:dev}"
E2E_HELPER_IMAGE="${E2E_HELPER_IMAGE:-decant-e2e-helper:dev}"
K3S_IMAGE="${K3S_IMAGE:-rancher/k3s:v1.35.1-k3s1}"
BUSYBOX_IMAGE="${BUSYBOX_IMAGE:-busybox:1.36}"
TOOLS_DIR="${TOOLS_DIR:-$PWD/.tools}"
export PATH="$TOOLS_DIR:$PATH"

up() {
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
    echo "E2E_DOOD: publishing k3s API on host bridge gateway ${gw}:6445"
    create_extra+=(--api-port "${gw}:6445")
  fi
  # ${arr[@]+"${arr[@]}"} guards the empty-array expansion: stock macOS bash 3.2 treats "${arr[@]}"
  # of an empty array as an unbound variable under `set -u` and aborts. No-op on bash 5 (CI).
  k3d cluster create "$CLUSTER" --servers 1 --agents 2 --image "$K3S_IMAGE" ${create_extra[@]+"${create_extra[@]}"} --wait --timeout 120s
  # busybox (inv-3 zombie-freeze ephemeral container) is multi-arch. With the host's
  # containerd image store the pulled manifest is an OCI index that `docker save` —
  # and hence `k3d image import` — can't export: it omits the other-arch blobs, which
  # fails the *whole* combined import (taking the app image down with it). Flatten it to
  # a single host-arch manifest under DooD CI. A classic image store stores single-arch
  # already, so a plain pull is fine there.
  if [ -n "${E2E_DOOD:-}" ]; then
    docker build -q -t "$BUSYBOX_IMAGE" - >/dev/null <<EOF
FROM $BUSYBOX_IMAGE
EOF
  else
    docker pull "$BUSYBOX_IMAGE" >/dev/null
  fi
  # 3-node import (simple + robust for a few small images; revisit a --registry-create if it gets slow).
  k3d image import "$IMAGE" "$E2E_HELPER_IMAGE" "$BUSYBOX_IMAGE" -c "$CLUSTER"
  export KUBECONFIG; KUBECONFIG="$(k3d kubeconfig write "$CLUSTER")"
  kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply -f test/e2e/manifests/
  helm install decant deploy/helm -n "$NS" -f test/e2e/values-e2e.yaml --wait --timeout 180s
  kubectl -n "$NS" rollout status deploy/decant --timeout 120s
}

down() { k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true; }

run_tests() {
  export KUBECONFIG; KUBECONFIG="$(k3d kubeconfig write "$CLUSTER")"
  export E2E_NAMESPACE="$NS"
  export KUBECTL="$TOOLS_DIR/kubectl"
  go test -tags e2e -count=1 -timeout 15m -v ./test/e2e/...
}

# Exercise the post-delete cleanup hook: a real `helm uninstall` (NOT the cluster-delete teardown,
# which bypasses helm) must leave no orphaned app-created HA objects (lease + checkpoint ConfigMap).
# Run AFTER run_tests so it doesn't disturb the failover suite. The leader-election lease is always
# present by now; the checkpoint ConfigMap is present once the leader has emitted at least once.
uninstall_verify() {
  export KUBECONFIG; KUBECONFIG="$(k3d kubeconfig write "$CLUSTER")"
  kubectl -n "$NS" get lease decant-leader >/dev/null 2>&1 \
    || { echo "PRE-FAIL: lease decant-leader absent before uninstall (cleanup test would be vacuous)" >&2; exit 1; }
  local had_cp="no"; kubectl -n "$NS" get configmap decant-checkpoints >/dev/null 2>&1 && had_cp="yes"
  echo "pre-uninstall: lease present, checkpoint present=${had_cp}"
  # --wait blocks on the post-delete hook Job (decant -cleanup) completing.
  helm uninstall decant -n "$NS" --wait --timeout 120s
  local fail=0
  if kubectl -n "$NS" get lease decant-leader >/dev/null 2>&1; then
    echo "FAIL: lease decant-leader orphaned after helm uninstall" >&2; fail=1
  fi
  if kubectl -n "$NS" get configmap decant-checkpoints >/dev/null 2>&1; then
    echo "FAIL: configmap decant-checkpoints orphaned after helm uninstall" >&2; fail=1
  fi
  [ "$fail" -eq 0 ] || exit 1
  echo "uninstall cleanup OK: post-delete hook removed the lease + checkpoint ConfigMap"
}

case "${1:-all}" in
  up) up ;;
  down) down ;;
  verify-cleanup) uninstall_verify ;;
  all) trap down EXIT; up; run_tests; uninstall_verify ;;
  *) echo "usage: $0 up|down|verify-cleanup|all"; exit 2 ;;
esac
