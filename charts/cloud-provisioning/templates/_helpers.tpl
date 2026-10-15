{{- define "cloud-provisioning.name" -}}
{{- default "cloud-provisioning" .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cloud-provisioning.controllerName" -}}
{{ include "cloud-provisioning.name" . }}-endpoint-controller
{{- end -}}

{{- define "cloud-provisioning.labels" -}}
app.kubernetes.io/name: {{ include "cloud-provisioning.name" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- /* Sanitize: a chart version may carry semver build metadata (Flux
appends the git sha, e.g. 0.1.0+e908348), and "+" is not legal in a
label value. */}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "cloud-provisioning.image" -}}
{{ include "cloud-provisioning.imageReference" (dict "image" .Values.image "defaultTag" .Chart.AppVersion) }}
{{- end -}}

{{- define "cloud-provisioning.dialerImage" -}}
{{ include "cloud-provisioning.imageReference" (dict "image" .Values.dialerImage "defaultTag" .Chart.AppVersion) }}
{{- end -}}

{{- define "cloud-provisioning.imageReference" -}}
{{- $digest := get .image "digest" -}}
{{- if not (kindIs "string" $digest) -}}
{{- fail "image digest must be a string" -}}
{{- end -}}
{{- if $digest -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $digest) -}}
{{- fail "image digest must be sha256 followed by 64 lowercase hexadecimal characters" -}}
{{- end -}}
{{- $repository := required "digest-pinned image requires a repository" .image.repository -}}
{{- if or (contains "@" $repository) (contains ":" (last (splitList "/" $repository))) -}}
{{- fail "digest-pinned image repository must not include a tag or digest" -}}
{{- end -}}
{{- printf "%s@%s" $repository $digest -}}
{{- else -}}
{{ .image.repository }}:{{ .image.tag | default .defaultTag }}
{{- end -}}
{{- end -}}

{{/* Host and port of the cluster API, split out of cluster.apiAddress
(https://host:port) so a CAPI controlPlaneEndpoint can be rendered
from the same single value the rest of the chart already takes. */}}
{{- define "cloud-provisioning.apiHost" -}}
{{- $hp := trimPrefix "https://" (trimPrefix "http://" (required "cluster.apiAddress is required" .Values.cluster.apiAddress)) -}}
{{- if contains "]" $hp }}{{ trimSuffix "]" (trimPrefix "[" (first (splitList "]" $hp))) }}{{ else }}{{ first (splitList ":" $hp) }}{{ end -}}
{{- end -}}

{{- define "cloud-provisioning.apiPort" -}}
{{- $hp := trimPrefix "https://" (trimPrefix "http://" .Values.cluster.apiAddress) -}}
{{- $tail := last (splitList ":" $hp) -}}
{{- if eq $tail $hp }}6443{{ else }}{{ $tail }}{{ end -}}
{{- end -}}
