{{/*
Shared template helpers for the genai-otel-bridge chart.

The chart deliberately deploys a single, fixed-name release (name: genai-otel-bridge) into one namespace —
it is not a general-purpose multi-tenant chart — so names stay hardcoded. These helpers exist only
to DRY the label blocks that were previously copy-pasted into every manifest.

IMPORTANT: genai-otel-bridge.selectorLabels is the immutable Deployment .spec.selector / PDB selector. It must
stay a stable, minimal subset (app: genai-otel-bridge) — never fold the version/instance/chart labels into it,
or a `helm upgrade` over an existing Deployment fails (selector is immutable post-create).
*/}}

{{- define "genai-otel-bridge.chart" -}}
{{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "genai-otel-bridge.selectorLabels" -}}
app: genai-otel-bridge
{{- end -}}

{{- define "genai-otel-bridge.labels" -}}
{{ include "genai-otel-bridge.selectorLabels" . }}
helm.sh/chart: {{ include "genai-otel-bridge.chart" . }}
app.kubernetes.io/name: genai-otel-bridge
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
