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

{{- /* Placement by mode, as JSON {sandboxNodes, system: {nodeSelector, tolerations}, scheduling, cilium}. A value set in
values.yaml (not null) replaces the mode's default whole: Helm would merge a map into a default map, never empty it. */ -}}
{{- define "sbx.placement" -}}
{{- $group := "node-restriction.kubernetes.io/proxcloud-node-group" -}}
{{- $d := dict -}}
{{- if include "sbx.shared" . -}}
{{- $d = dict
  "sandboxNodes" (dict $group "sandbox")
  "system" (dict "nodeSelector" (dict $group "system") "tolerations" list)
  "scheduling" (dict "nodeSelector" (dict $group "sandbox") "tolerations" (list (dict "key" "proxcloud.io/sandbox" "operator" "Exists" "effect" "NoSchedule")))
  "cilium" (dict "proxcloud.io/node-group" "sandbox") -}}
{{- else -}}
{{- $d = dict
  "sandboxNodes" dict
  "system" (dict "nodeSelector" (dict "node-restriction.kubernetes.io/proxcloud-system" "true") "tolerations" (list (dict "key" "proxcloud.io/sandbox-system" "operator" "Exists" "effect" "NoSchedule")))
  "scheduling" dict
  "cilium" dict -}}
{{- end -}}
{{- $v := .Values -}}
{{- if not (kindIs "invalid" $v.sandboxNodes.nodeSelector) }}{{ $_ := set $d "sandboxNodes" $v.sandboxNodes.nodeSelector }}{{ end -}}
{{- if not (kindIs "invalid" $v.system.nodeSelector) }}{{ $_ := set $d.system "nodeSelector" $v.system.nodeSelector }}{{ end -}}
{{- if not (kindIs "invalid" $v.system.tolerations) }}{{ $_ := set $d.system "tolerations" $v.system.tolerations }}{{ end -}}
{{- if not (kindIs "invalid" $v.runtimeClass.scheduling) }}{{ $_ := set $d "scheduling" $v.runtimeClass.scheduling }}{{ end -}}
{{- if not (kindIs "invalid" $v.cilium.nodeSelector) }}{{ $_ := set $d "cilium" $v.cilium.nodeSelector }}{{ end -}}
{{- toJson $d -}}
{{- end -}}
