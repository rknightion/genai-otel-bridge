{{/*
Shared template helpers for the aip-oi chart.

The chart deliberately deploys a single, fixed-name release (name: aip-oi) into one namespace —
it is not a general-purpose multi-tenant chart — so names stay hardcoded. These helpers exist only
to DRY the label blocks that were previously copy-pasted into every manifest.

IMPORTANT: aip-oi.selectorLabels is the immutable Deployment .spec.selector / PDB selector. It must
stay a stable, minimal subset (app: aip-oi) — never fold the version/instance/chart labels into it,
or a `helm upgrade` over an existing Deployment fails (selector is immutable post-create).
*/}}

{{- define "aip-oi.chart" -}}
{{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "aip-oi.selectorLabels" -}}
app: aip-oi
{{- end -}}

{{- define "aip-oi.labels" -}}
{{ include "aip-oi.selectorLabels" . }}
helm.sh/chart: {{ include "aip-oi.chart" . }}
app.kubernetes.io/name: aip-oi
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
