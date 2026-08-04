{{- define "cloud-provisioning.name" -}}
{{- default "wg-dialer" .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
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
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "cloud-provisioning.dialerImage" -}}
{{ .Values.dialerImage.repository }}:{{ .Values.dialerImage.tag | default .Chart.AppVersion }}
{{- end -}}
