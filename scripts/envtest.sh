#!/usr/bin/env bash
# Layer-1 integration tests against a real kube-apiserver+etcd (envtest) — no kubelet/cluster.
# Proves Lease election + ConfigMap RMW under real optimistic-concurrency semantics.
set -euo pipefail
TOOLS_DIR="${TOOLS_DIR:-$PWD/.tools}"
ENVTEST_K8S_VERSION="${ENVTEST_K8S_VERSION:-1.35.0}"
ASSETS="$("$TOOLS_DIR/setup-envtest" use "$ENVTEST_K8S_VERSION" --bin-dir "$TOOLS_DIR/envtest" -p path)"
export KUBEBUILDER_ASSETS="$ASSETS"
echo "envtest assets: $KUBEBUILDER_ASSETS"
go test -tags envtest -count=1 ./test/integration/...
