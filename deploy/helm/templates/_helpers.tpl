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

{{/*
Container image shared by the Deployment and the post-delete cleanup Job. Default to the image built for
THIS chart release when .Values.image is unset: chart version == appVersion == release version ==
published image tag (all unprefixed, e.g. 3.0.1). A chart packaged for release X.Y.Z therefore
default-installs the X.Y.Z image, never the previous release's binary. Override .Values.image to pin a
digest or use a mirror/proxy.
*/}}
{{- define "genai-otel-bridge.image" -}}
{{ .Values.image | default (printf "ghcr.io/rknightion/genai-otel-bridge:%s" .Chart.AppVersion) }}
{{- end -}}

{{/*
Tenant ids, as a JSON list, from whichever config the ConfigMap renders: the parsed configOverride when
set, else config.tenants. A configOverride that does not parse fails the render here, not at pod start.
An id becomes a Secret name and a mount path, so it must match the tenant id rule of the config schema.
*/}}
{{- define "genai-otel-bridge.tenantIDs" -}}
{{- $tenants := .Values.config.tenants -}}
{{- if .Values.configOverride -}}
{{- $parsed := fromYaml .Values.configOverride -}}
{{- if hasKey $parsed "Error" -}}
{{- fail (printf "configOverride does not parse as a YAML map: %s" $parsed.Error) -}}
{{- end -}}
{{- $tenants = $parsed.tenants -}}
{{- end -}}
{{- if not $tenants -}}
{{- $tenants = dict -}}
{{- end -}}
{{- if not (kindIs "map" $tenants) -}}
{{- fail "tenants must be a map keyed by tenant id" -}}
{{- end -}}
{{- range $id := keys $tenants -}}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$" $id) -}}
{{- fail (printf "tenant id %q must match [a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?" $id) -}}
{{- end -}}
{{- end -}}
{{- keys $tenants | sortAlpha | toJson -}}
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
