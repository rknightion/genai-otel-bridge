{{/*
Shared template helpers for the decant chart.

The chart deliberately deploys a single, fixed-name release (name: decant) into one namespace —
it is not a general-purpose multi-tenant chart — so names stay hardcoded. These helpers exist only
to DRY the label blocks that were previously copy-pasted into every manifest.

IMPORTANT: decant.selectorLabels is the immutable Deployment .spec.selector / PDB selector. It must
stay a stable, minimal subset (app: decant) — never fold the version/instance/chart labels into it,
or a `helm upgrade` over an existing Deployment fails (selector is immutable post-create).
*/}}

{{- define "decant.chart" -}}
{{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "decant.selectorLabels" -}}
app: decant
{{- end -}}

{{- define "decant.labels" -}}
{{ include "decant.selectorLabels" . }}
helm.sh/chart: {{ include "decant.chart" . }}
app.kubernetes.io/name: decant
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
