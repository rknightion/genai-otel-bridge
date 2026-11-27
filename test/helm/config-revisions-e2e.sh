#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Run by scripts/k3d-e2e.sh on its existing throwaway cluster, after the HA and tenant tests.
set -euo pipefail
ns=${1:?pass the k3d test namespace}
bridge_image=${2:?pass the locally built bridge image}
helper_image=${3:?pass the locally built helper image}
reader_image=${4:?pass the imported utility image}
release=genai-otel-bridge
scratch=$(mktemp -d)
k() { kubectl -n "$ns" --request-timeout=15s "$@"; }
h() { helm -n "$ns" "$@"; }
cleanup() {
  if [ "$?" -ne 0 ]; then k logs job/genai-otel-bridge-config-revisions >&2 || true; fi
  rm -rf "$scratch"
}
trap cleanup EXIT
k create namespace "$ns"
k apply -f test/e2e/manifests/secrets.yaml

values=(-f test/e2e/values-e2e.yaml --set "image=$bridge_image"
  --set "extraContainers[0].image=$helper_image" --set "extraContainers[1].image=$helper_image"
  --set 'extraContainers[2].name=config-reader' --set "extraContainers[2].image=$reader_image"
  --set 'extraContainers[2].imagePullPolicy=IfNotPresent'
  --set-json 'extraContainers[2].command=["tail","-f","/dev/null"]'
  --set-json 'extraContainers[2].volumeMounts=[{"name":"config","mountPath":"/config","readOnly":true}]'
  --set 'replicas=1,revisionHistoryLimit=0,rollingUpdate.maxUnavailable=0,terminationGraceSeconds=5')
h install "$release" deploy/helm "${values[@]}" --wait --timeout 120s
pod_mount='{.spec.volumes[?(@.name=="config")].configMap.name}'
deployment_mount='{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}'
old_pod=$(k get pods -l "app=$release" -o jsonpath='{.items[0].metadata.name}')
old_map=$(k get pod "$old_pod" -o "jsonpath=$pod_mount")
old_uid=$(k get cm "$old_map" -o jsonpath='{.metadata.uid}')
old_rs=$(k get pod "$old_pod" -o jsonpath='{.metadata.ownerReferences[0].name}')
old_hash_label=$(k get rs "$old_rs" -o jsonpath='{.spec.selector.matchLabels.pod-template-hash}')
k exec "$old_pod" -c config-reader -- cat /config/config.yaml >"$scratch/valid.yaml"
test -s "$scratch/valid.yaml"
cp "$scratch/valid.yaml" "$scratch/bad.yaml"
printf '\nunknown_config_field: true\n' >>"$scratch/bad.yaml"

# Give the opted-in hook two unused maps. Creation order and the name tie-breaker agree even
# when the API timestamps match. A foreign map with the same labels is never a candidate.
old_unused="genai-otel-bridge-config-$(printf '%064d' 1)"
new_unused="genai-otel-bridge-config-$(printf '%064d' 2)"
foreign="genai-otel-bridge-config-$(printf '%064d' 3)"
for name in "$old_unused" "$new_unused" "$foreign"; do
  owner=$release
  if [ "$name" = "$foreign" ]; then owner=another-release; fi
  k create cm "$name" --from-literal=config.yaml='example: true'
  k label cm "$name" app.kubernetes.io/component=config-revision
  k annotate cm "$name" helm.sh/resource-policy=keep \
    "meta.helm.sh/release-name=$owner" "meta.helm.sh/release-namespace=$ns"
done
absent() {
  local result
  result=$(k get cm "$1" --ignore-not-found -o name)
  [ -z "$result" ] || { echo "FAIL: unexpected retained ConfigMap $result" >&2; return 1; }
}

# Only wait for the hook; the bad Deployment must never become Ready.
h upgrade "$release" deploy/helm "${values[@]}" --set-file "configOverride=$scratch/bad.yaml" \
  --set configRevisionPruning.enabled=true,configRevisionPruning.historyLimit=1 --wait=hookOnly --timeout 120s
new_map=$(k get deployment "$release" -o "jsonpath=$deployment_mount")
[ "$old_map" != "$new_map" ]
absent "$old_unused"
k get cm "$old_map" "$new_map" "$new_unused" "$foreign"
if k rollout status "deployment/$release" --timeout=12s; then
  echo 'FAIL: rollout with invalid config became Ready' >&2; exit 1
fi
bad_rs=$(k get rs -l "app=$release" -o name | grep -vxF "replicaset.apps/$old_rs")
bad_hash_label=$(k get "$bad_rs" -o jsonpath='{.spec.selector.matchLabels.pod-template-hash}')
k wait pod -l "pod-template-hash=$bad_hash_label" \
  --for='jsonpath={.status.containerStatuses[?(@.name=="genai-otel-bridge")].state.waiting.reason}=CrashLoopBackOff' --timeout=45s
bad_pod=$(k get pods -l "pod-template-hash=$bad_hash_label" -o jsonpath='{.items[0].metadata.name}')
bad_logs=$(k logs "$bad_pod" -c genai-otel-bridge --previous)
grep -Fq 'unknown_config_field' <<<"$bad_logs"
k wait "pod/$old_pod" --for=condition=Ready --timeout=15s
k delete "pod/$old_pod" --wait=false
replacement=(-l "pod-template-hash=$old_hash_label" --field-selector "metadata.name!=$old_pod")
k wait pod "${replacement[@]}" --for=create --timeout=30s
k wait pod "${replacement[@]}" --for=condition=Ready --timeout=45s
new_pod=$(k get pod "${replacement[@]}" -o jsonpath='{.items[0].metadata.name}')
[ "$new_pod" != "$old_pod" ]
[ "$(k get pod "$new_pod" -o "jsonpath=$pod_mount")" = "$old_map" ]
[ "$(k get cm "$old_map" -o jsonpath='{.metadata.uid}')" = "$old_uid" ]
k exec "$new_pod" -c config-reader -- cat /config/config.yaml >"$scratch/restarted.yaml"
cmp "$scratch/valid.yaml" "$scratch/restarted.yaml"
echo 'AC1: replacement Pod from the old ReplicaSet became Ready and mounted the original bytes'

# Reuse the last good values, then prune once Kubernetes has removed the bad ReplicaSet and Pods.
h upgrade "$release" deploy/helm "${values[@]}" --wait --timeout 120s
[ "$(k get deployment "$release" -o "jsonpath=$deployment_mount")" = "$old_map" ]
k wait "$bad_rs" --for=delete --timeout=45s
bad_pods=$(k get pods -l "pod-template-hash=$bad_hash_label" -o name)
while IFS= read -r pod; do
  if [ -n "$pod" ]; then k wait "$pod" --for=delete --timeout=45s; fi
done <<<"$bad_pods"
k get cm "$new_map" "$new_unused"  # the default-off recovery upgrade didn't prune
h upgrade "$release" deploy/helm "${values[@]}" --set configRevisionPruning.enabled=true,configRevisionPruning.historyLimit=0 \
  --wait --timeout 120s
absent "$new_map"
absent "$new_unused"
k get cm "$old_map" "$foreign"
echo 'AC3: limits 1 and 0 pruned only unused, owned revisions; default-off recovery kept them'
