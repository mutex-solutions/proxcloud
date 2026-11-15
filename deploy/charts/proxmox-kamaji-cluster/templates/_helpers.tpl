{{- /* "true" when controlPlane.kind is kubeadm (the platform's own cluster), "" for kamaji (a tenant cluster). */ -}}
{{- define "pkc.kubeadm" -}}
{{- $kind := (.Values.controlPlane | default dict).kind | default "kamaji" -}}
{{- if not (has $kind (list "kamaji" "kubeadm")) -}}
{{- fail (printf "controlPlane.kind %q: kamaji or kubeadm" $kind) -}}
{{- end -}}
{{- if eq $kind "kubeadm" }}true{{ end -}}
{{- end -}}
