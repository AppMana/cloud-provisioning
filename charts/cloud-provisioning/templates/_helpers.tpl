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
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "cloud-provisioning.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "cloud-provisioning.dialerImage" -}}
{{ .Values.dialerImage.repository }}:{{ .Values.dialerImage.tag | default .Chart.AppVersion }}
{{- end -}}
