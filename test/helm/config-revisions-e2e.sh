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
trap 'rm -rf "$scratch"' EXIT
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

# A revision-labelled map of another release must survive the documented clean-up.
foreign="genai-otel-bridge-config-$(printf '%064d' 3)"
k create cm "$foreign" --from-literal=config.yaml='example: true'
k label cm "$foreign" app.kubernetes.io/component=config-revision app.kubernetes.io/instance=another-release

# Wait only for hooks: the bad Deployment must never become Ready.
h upgrade "$release" deploy/helm "${values[@]}" --set-file "configOverride=$scratch/bad.yaml" \
  --wait=hookOnly --timeout 120s
new_map=$(k get deployment "$release" -o "jsonpath=$deployment_mount")
[ "$old_map" != "$new_map" ]
k get cm "$old_map" "$new_map"
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

# Restore the last good values. Helm keeps the bad revision although it left the release manifest.
h upgrade "$release" deploy/helm "${values[@]}" --wait --timeout 120s
[ "$(k get deployment "$release" -o "jsonpath=$deployment_mount")" = "$old_map" ]
k wait "$bad_rs" --for=delete --timeout=45s
bad_pods=$(k get pods -l "pod-template-hash=$bad_hash_label" -o name)
while IFS= read -r pod; do
  if [ -n "$pod" ]; then k wait "$pod" --for=delete --timeout=45s; fi
done <<<"$bad_pods"
k get cm "$old_map" "$new_map" "$foreign"

# The manual clean-up in docs/installation.md (Cleaning up old revisions). config_revisions.py
# checks that the block between these markers matches the documented one.
pushd "$scratch" >/dev/null
# >>> docs clean-up block
(
  set -euo pipefail
  rm -f in-use.txt unused.txt
  # Each read stops the block on any error, so a partial read cannot mark a revision unused.
  mounted=$(kubectl -n "$ns" get deployment genai-otel-bridge \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')
  refs=$(kubectl -n "$ns" get deployments,replicasets,pods \
    -o jsonpath='{..configMap.name} {..configMapRef.name} {..configMapKeyRef.name}{"\n"}')
  revisions=$(kubectl -n "$ns" get configmaps -o name \
    -l "app.kubernetes.io/component=config-revision,app.kubernetes.io/instance=$release")
  tr ' ' '\n' <<<"$refs" | sed '/^$/d' | sort -u >in-use.txt
  if [ -z "$mounted" ] || ! grep -qxF "$mounted" in-use.txt; then
    echo "stop: the map the Deployment mounts is not in in-use.txt" >&2; exit 1
  fi
  sed -e 's|^configmap/||' -e '/^$/d' <<<"$revisions" \
    | { grep -vxF -f in-use.txt || [ "$?" -eq 1 ]; } >unused.txt
  cat in-use.txt unused.txt
)
# <<< docs clean-up block
[ "$(cat unused.txt)" = "$new_map" ] || { echo "FAIL: clean-up listed: $(cat unused.txt)" >&2; exit 1; }
xargs -r kubectl -n "$ns" delete configmap <unused.txt
popd >/dev/null
k wait "cm/$new_map" --for=delete --timeout=30s
k get cm "$old_map" "$foreign"
k rollout status "deployment/$release" --timeout=30s
echo 'clean-up: the documented commands removed only the unused revision of this release'
