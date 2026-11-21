{{- /* "true" in mode shared, "" in mode separate; any other mode fails the render. */ -}}
{{- define "sbx.shared" -}}
{{- if not (has .Values.mode (list "shared" "separate")) -}}
{{- fail (printf "mode %q: shared or separate" .Values.mode) -}}
{{- end -}}
{{- if eq .Values.mode "shared" }}true{{ end -}}
{{- end -}}

{{- /* The service account user name of one of the release namespace's identities. */ -}}
{{- define "sbx.sa" -}}
system:serviceaccount:{{ .root.Release.Namespace }}:{{ .name }}
{{- end -}}
