#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Chart regression. Requires k3d, Docker, kubectl, Helm and the two images from just image-local.
set -euo pipefail

cluster="bridge-config-$$"
release=genai-otel-bridge
k3s_image=${K3S_IMAGE:?set K3S_IMAGE or use just e2e}
bridge_image=${IMAGE:-genai-otel-bridge:dev}
helper_image=${E2E_HELPER_IMAGE:-genai-otel-bridge-e2e-helper:dev}
helm=${HELM:-helm}
export PATH="${TOOLS_DIR:-$PWD/.tools}:$PATH"
scratch=$(mktemp -d)
cleanup() {
  if [ "$?" -ne 0 ] && [ -s "$KUBECONFIG" ]; then
    kubectl --request-timeout=5s logs job/genai-otel-bridge-config-revisions >&2 || true
    kubectl --request-timeout=5s get events --field-selector type=Warning >&2 || true
  fi
  k3d cluster delete "$cluster" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT
trap 'exit 1' INT TERM
export KUBECONFIG="$scratch/kubeconfig"
kubectl() { command kubectl --request-timeout=15s "$@"; }

create_extra=()
if [ -n "${E2E_DOOD:-}" ]; then
  gw=$(docker network inspect bridge --format '{{ (index .IPAM.Config 0).Gateway }}')
  [ -n "$gw" ] || { echo 'E2E_DOOD: no Docker bridge gateway' >&2; exit 1; }
  create_extra+=(--api-port "$gw:${E2E_API_PORT:-6445}")
fi
k3d cluster create "$cluster" --image "$k3s_image" --wait --timeout 90s \
  --kubeconfig-update-default=false --kubeconfig-switch-context=false ${create_extra[@]+"${create_extra[@]}"}
k3d kubeconfig get "$cluster" >"$KUBECONFIG"
k3d image import "$bridge_image" "$helper_image" -c "$cluster"
kubectl apply -f test/e2e/manifests/secrets.yaml

helm_values=(-f test/e2e/values-e2e.yaml --set "image=$bridge_image" \
  --set "extraContainers[0].image=$helper_image" --set "extraContainers[1].image=$helper_image" \
  --set replicas=1 --set revisionHistoryLimit=0 --set rollingUpdate.maxUnavailable=0 \
  --set configRevisionHistoryLimit=0 --set terminationGraceSeconds=5 --set cleanup.retainCheckpoint=true)
chart=${CHART:-deploy/helm}

# Re-use the existing offline bridge config (all upstream traffic is to sidecars), but add a bad
# field to the file for the stopped rollout. A post-upgrade hook also runs with this wait strategy.
"$helm" template "$release" "$chart" "${helm_values[@]}" --show-only templates/configmap.yaml |
  kubectl create --dry-run=client -f - -o jsonpath='{.data.config\.yaml}' >"$scratch/valid.yaml"
cp "$scratch/valid.yaml" "$scratch/invalid.yaml"
printf '\nunknown_config_field: true\n' >>"$scratch/invalid.yaml"

# The optional first install from the old chart exercises fixed-name migration as well.
# Seed maps that must never be in the delete grant, even if their names or labels resemble revisions.
foreign="genai-otel-bridge-config-$(printf '%064d' 1)"
late="genai-otel-bridge-config-$(printf '%064d' 2)"
make_map() {
  kubectl create configmap "$1" --from-literal=config.yaml='example: true'
  kubectl label configmap "$1" "app.kubernetes.io/instance=$release" app.kubernetes.io/component=config-revision
  kubectl annotate configmap "$1" helm.sh/resource-policy=keep \
    "meta.helm.sh/release-name=$2" meta.helm.sh/release-namespace=default
}
make_map unrelated "$release"
make_map "$foreign" another-release
if [ -z "${FIRST_CHART:-}" ]; then make_map genai-otel-bridge-config another-release; fi

"$helm" install "$release" "${FIRST_CHART:-$chart}" "${helm_values[@]}" --wait --timeout 90s

kubectl_json() { kubectl get "$1" "$2" -o "jsonpath=$3"; }
config_mount='{.spec.volumes[?(@.name=="config")].configMap.name}'
old_pod=$(kubectl get pods -l "app=$release" -o jsonpath='{.items[0].metadata.name}')
old_map=$(kubectl_json pod "$old_pod" "$config_mount")
old_uid=$(kubectl_json configmap "$old_map" '{.metadata.uid}')
old_rs=$(kubectl_json pod "$old_pod" '{.metadata.ownerReferences[0].name}')
old_hash=$(kubectl_json configmap "$old_map" '{.data.config\.yaml}' | sha256sum)

"$helm" upgrade "$release" "$chart" "${helm_values[@]}" \
  --set-file "configOverride=$scratch/invalid.yaml" --timeout 90s
new_map=$(kubectl_json deployment "$release" '{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')

# Helm removes successful hook resources. Replay only the recorded Role and RoleBinding to
# check actual Kubernetes authorisation without running the Job again or copying its rules.
"$helm" get hooks "$release" | awk '
  function flush() { if (keep) printf "---\n%s", yaml; yaml=""; keep=0; rbac=0 }
  /^---$/ { flush(); next }
  { yaml=yaml $0 "\n" }
  /^kind: (Role|RoleBinding)$/ { rbac=1; next }
  /^kind: / { rbac=0 }
  rbac && /^  name: genai-otel-bridge-config-revisions$/ { keep=1 }
  END { flush() }
' | kubectl apply -f -
can_pruner() {
  kubectl auth can-i --as system:serviceaccount:default:genai-otel-bridge-config-revisions \
    --as-group system:serviceaccounts --as-group system:serviceaccounts:default \
    --as-group system:authenticated "$@"
}
can_pruner delete "configmaps/$old_map"
can_pruner delete "configmaps/$new_map"
for name in unrelated genai-otel-bridge-checkpoints "$foreign" "$late"; do
  [ "$(can_pruner delete "configmaps/$name" || true)" = no ] || {
    echo "FAIL: expected RBAC denial on ConfigMap/$name" >&2; exit 1;
  }
done
if [ -z "${FIRST_CHART:-}" ]; then
  [ "$(can_pruner delete configmaps/genai-otel-bridge-config || true)" = no ]
fi
[ "$(can_pruner deletecollection configmaps || true)" = no ]
kubectl delete rolebinding,role genai-otel-bridge-config-revisions
echo 'ConfigMap RBAC: delete granted for the discovered old and rendered new names; denied for others'
[ "$old_map" != "$new_map" ] || { echo 'FAIL: stopped rollout reused the old ConfigMap'; exit 1; }
if kubectl rollout status "deployment/$release" --timeout=15s; then
  echo 'FAIL: rollout with invalid config became Ready'; exit 1
fi
kubectl wait "pod/$old_pod" --for=condition=Ready --timeout=10s
kubectl delete "pod/$old_pod" --wait=false
hash_label=$(kubectl_json replicaset "$old_rs" '{.spec.selector.matchLabels.pod-template-hash}')
replacement=(-l "pod-template-hash=$hash_label" --field-selector "metadata.name!=$old_pod")
kubectl wait pod "${replacement[@]}" --for=create --timeout=30s
kubectl wait pod "${replacement[@]}" --for=condition=Ready --timeout=45s
restarted=$(kubectl get pod "${replacement[@]}" -o jsonpath='{.items[0].metadata.name}')
[ "$old_pod" != "$restarted" ]
[ "$(kubectl_json pod "$restarted" "$config_mount")" = "$old_map" ]
[ "$(kubectl_json configmap "$old_map" '{.metadata.uid}')" = "$old_uid" ]
[ "$(kubectl_json configmap "$old_map" '{.data.config\.yaml}' | sha256sum)" = "$old_hash" ]
echo 'AC1: replacement Pod from the old ReplicaSet is Ready on the original ConfigMap bytes'

# Recover, then wait out Kubernetes ReplicaSet history cleanup before the next live prune.
bad_rs=$(kubectl get rs -l "app=$release" -o name | grep -vxF "replicaset.apps/$old_rs")
if [ "${ROLLBACK_VIA_UPGRADE:-0}" = 1 ]; then
  "$helm" upgrade "$release" "$chart" --reset-values "${helm_values[@]}" \
    --set-file "configOverride=$scratch/valid.yaml" --wait --timeout 90s
  restored=$(kubectl_json deployment "$release" '{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')
  [ "$(kubectl_json configmap "$restored" '{.data.config\.yaml}' | sha256sum)" = "$(sha256sum <"$scratch/valid.yaml")" ]
  echo 'Upgrade using the last good config restored the previous bytes'
else
  "$helm" rollback "$release" 1 --wait --timeout 90s
  [ "$(kubectl_json deployment "$release" '{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')" = "$old_map" ]
  [ "$(kubectl_json configmap "$old_map" '{.metadata.uid}')" = "$old_uid" ]
  kubectl get configmap "$new_map" >/dev/null
  echo 'Native rollback reused the previous ConfigMap without deleting the newer one'
fi
kubectl wait "$bad_rs" --for=delete --timeout=45s
"$helm" upgrade "$release" "$chart" "${helm_values[@]}" --wait --timeout 90s
[ -z "$(kubectl get configmap "$new_map" --ignore-not-found -o name)" ]
current=$(kubectl_json deployment "$release" '{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')
kubectl get configmap "$current" >/dev/null
echo 'AC3: history limit 0 pruned the unreferenced revision, not the current one'

# Uninstall replays the recorded hook. A revision that arrived later must not be deleted by it.
make_map "$late" "$release"
"$helm" uninstall "$release" --cascade=foreground --wait --timeout 90s
[ -z "$(kubectl get configmap "$current" --ignore-not-found -o name)" ]
[ -z "$(kubectl get configmap "$old_map" --ignore-not-found -o name)" ]
kubectl get configmap unrelated "$foreign" "$late" >/dev/null
if [ -z "${FIRST_CHART:-}" ]; then kubectl get configmap genai-otel-bridge-config >/dev/null; fi
echo 'Uninstall removed the recorded revisions and left foreign and unrecorded maps alone'
