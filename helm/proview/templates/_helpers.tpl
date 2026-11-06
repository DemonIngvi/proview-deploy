{{- define "proview.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: proview
{{- end -}}

{{- define "proview.secretName" -}}
{{- default .Values.secrets.name .Values.secrets.existingSecret -}}
{{- end -}}

{{- define "proview.storageClass" -}}
{{- if . }}
storageClass: {{ . }}
{{- end }}
{{- end -}}
