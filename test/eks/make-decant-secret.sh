#!/usr/bin/env bash
# Create/update the `decant-secrets` Secret that test/eks/values-eks.yaml's ${ENV} refs resolve from.
#
# This script contains NO secrets — it reads them from the environment (or an optional dotenv you point
# DECANT_SECRET_ENV at) and writes the env-var keys the values file expects. Provide:
#   GC_OTLP_ENDPOINT, GC_INSTANCE_ID, GC_OTLP_TOKEN              product-plane Grafana Cloud OTLP creds
#   GC_SELF_OTLP_ENDPOINT, GC_SELF_OTLP_INSTANCE_ID, GC_SELF_OTLP_TOKEN   self-obs OTLP creds
#   GC_PYROSCOPE_URL, GC_PYROSCOPE_USER, GC_PYROSCOPE_PASSWORD  self-profiling push (Pyroscope)
#   PORTKEY_API_KEY                                             Portkey source key
#   PORTKEY_EXPECTED_WORKSPACE                                  Portkey workspace SLUG (analytics/groups scope assertion)
#   PORTKEY_WORKSPACE_ID                                        Portkey workspace UUID (logs_export)
#   LANGSMITH_API_KEY, LANGSMITH_BASE_URL                       LangSmith source key + API base (must include /api/v1)
#
# Usage:  ./test/eks/make-decant-secret.sh [namespace]   (default namespace: default)
#         DECANT_SECRET_ENV=./my.env ./test/eks/make-decant-secret.sh   (load vars from a dotenv first)
set -euo pipefail

NS="${1:-default}"

if [ -n "${DECANT_SECRET_ENV:-}" ]; then
  [ -f "$DECANT_SECRET_ENV" ] || { echo "DECANT_SECRET_ENV=$DECANT_SECRET_ENV not found" >&2; exit 1; }
  set -a
  # shellcheck disable=SC1090
  . "$DECANT_SECRET_ENV"
  set +a
fi

# Fail loudly if any required value is missing (mirrors the binary's fatal-on-unset-${ENV}).
: "${GC_OTLP_ENDPOINT:?}" "${GC_INSTANCE_ID:?}" "${GC_OTLP_TOKEN:?}"
: "${GC_SELF_OTLP_ENDPOINT:?}" "${GC_SELF_OTLP_INSTANCE_ID:?}" "${GC_SELF_OTLP_TOKEN:?}"
: "${GC_PYROSCOPE_URL:?}" "${GC_PYROSCOPE_USER:?}" "${GC_PYROSCOPE_PASSWORD:?}"
: "${PORTKEY_API_KEY:?}"
: "${PORTKEY_EXPECTED_WORKSPACE:?the Portkey workspace slug for the analytics/groups scope assertion}"
: "${PORTKEY_WORKSPACE_ID:?the Portkey workspace UUID used by logs_export}"
: "${LANGSMITH_API_KEY:?}"
: "${LANGSMITH_BASE_URL:?the LangSmith API base, must include /api/v1}"

kubectl create secret generic decant-secrets -n "$NS" \
  --from-literal=GC_OTLP_ENDPOINT="$GC_OTLP_ENDPOINT" \
  --from-literal=GC_INSTANCE_ID="$GC_INSTANCE_ID" \
  --from-literal=GC_OTLP_TOKEN="$GC_OTLP_TOKEN" \
  --from-literal=GC_SELF_OTLP_ENDPOINT="$GC_SELF_OTLP_ENDPOINT" \
  --from-literal=GC_SELF_OTLP_INSTANCE_ID="$GC_SELF_OTLP_INSTANCE_ID" \
  --from-literal=GC_SELF_OTLP_TOKEN="$GC_SELF_OTLP_TOKEN" \
  --from-literal=GC_PYROSCOPE_URL="$GC_PYROSCOPE_URL" \
  --from-literal=GC_PYROSCOPE_USER="$GC_PYROSCOPE_USER" \
  --from-literal=GC_PYROSCOPE_PASSWORD="$GC_PYROSCOPE_PASSWORD" \
  --from-literal=PORTKEY_API_KEY="$PORTKEY_API_KEY" \
  --from-literal=PORTKEY_EXPECTED_WORKSPACE="$PORTKEY_EXPECTED_WORKSPACE" \
  --from-literal=PORTKEY_WORKSPACE_ID="$PORTKEY_WORKSPACE_ID" \
  --from-literal=LANGSMITH_API_KEY="$LANGSMITH_API_KEY" \
  --from-literal=LANGSMITH_BASE_URL="$LANGSMITH_BASE_URL" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "decant-secrets applied to namespace '$NS' (14 keys)"
