{{/*
Minimal helper set: name/fullname/labels/selector + connection builders.
*/}}

{{- define "proxcloud.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "proxcloud.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains (include "proxcloud.name" .) .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "proxcloud.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{/* Common labels (Kubernetes recommended set). */}}
{{- define "proxcloud.labels" -}}
app.kubernetes.io/name: {{ include "proxcloud.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: proxcloud
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{/*
Selector labels for one component.
Usage: include "proxcloud.selector" (dict "ctx" $ "component" "api")
*/}}
{{- define "proxcloud.selector" -}}
app.kubernetes.io/name: {{ include "proxcloud.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/* The Secret the api consumes (chart-managed unless api.existingSecret). */}}
{{- define "proxcloud.apiSecretName" -}}
{{- if .Values.api.existingSecret -}}
{{- .Values.api.existingSecret -}}
{{- else -}}
{{- printf "%s-api" (include "proxcloud.fullname" .) -}}
{{- end -}}
{{- end }}

{{/* CloudNativePG cluster name (the app connects to <name>-rw). */}}
{{- define "proxcloud.cnpgClusterName" -}}
{{- .Values.cnpg.clusterName | default (printf "%s-pg" (include "proxcloud.fullname" .)) -}}
{{- end }}

{{/* CloudNativePG owner-credentials Secret (basic-auth: username/password). */}}
{{- define "proxcloud.cnpgSecretName" -}}
{{- .Values.cnpg.existingSecret | default (printf "%s-cnpg-app" (include "proxcloud.fullname" .)) -}}
{{- end }}

{{/* PostgreSQL host: CloudNativePG -rw service, else bundled subchart service, else external host. */}}
{{- define "proxcloud.pgHost" -}}
{{- if .Values.cnpg.enabled -}}
{{- printf "%s-rw" (include "proxcloud.cnpgClusterName" .) -}}
{{- else if .Values.postgresql.enabled -}}
{{- /* honor the subchart's fullnameOverride — that IS its service name */ -}}
{{- .Values.postgresql.fullnameOverride | default (printf "%s-postgresql" .Release.Name) -}}
{{- else -}}
{{- required "externalPostgresql.host is required when postgresql.enabled=false" .Values.externalPostgresql.host -}}
{{- end -}}
{{- end }}

{{/*
PROXCLOUD_DB_URL builder. Only used when the DSN is chart-derived (i.e.
externalPostgresql.existingSecret is empty). Credentials are URL-encoded.
*/}}
{{- define "proxcloud.dbURL" -}}
{{- /* urlquery encodes space as "+", which URL userinfo parsing keeps literal — force %20. */ -}}
{{- if .Values.cnpg.enabled -}}
{{- $u := .Values.postgresql.auth.username | urlquery | replace "+" "%20" -}}
{{- $p := .Values.postgresql.auth.password | required "postgresql.auth.password is required (CloudNativePG owner) — set a strong password or use cnpg.existingSecret; for dev use -f deploy/charts/proxcloud/values-dev.yaml" | urlquery | replace "+" "%20" -}}
postgres://{{ $u }}:{{ $p }}@{{ include "proxcloud.pgHost" . }}:5432/{{ .Values.postgresql.auth.database }}?sslmode=require
{{- else if .Values.postgresql.enabled -}}
{{- $u := .Values.postgresql.auth.username | urlquery | replace "+" "%20" -}}
{{- $p := .Values.postgresql.auth.password | required "postgresql.auth.password is required (bundled PostgreSQL) — set a strong password or use externalPostgresql.existingSecret; for dev use -f deploy/charts/proxcloud/values-dev.yaml" | urlquery | replace "+" "%20" -}}
postgres://{{ $u }}:{{ $p }}@{{ include "proxcloud.pgHost" . }}:5432/{{ .Values.postgresql.auth.database }}?sslmode=disable
{{- else -}}
{{- $u := .Values.externalPostgresql.username | urlquery | replace "+" "%20" -}}
{{- $p := .Values.externalPostgresql.password | urlquery | replace "+" "%20" -}}
postgres://{{ $u }}:{{ $p }}@{{ include "proxcloud.pgHost" . }}:{{ .Values.externalPostgresql.port }}/{{ .Values.externalPostgresql.database }}?sslmode={{ .Values.externalPostgresql.sslMode }}
{{- end -}}
{{- end }}

{{/* Target Keycloak base URL for keycloak-config-cli: explicit url, else the
bundled keycloakx http service (keycloakx names it <fullname>-http). */}}
{{- define "proxcloud.kcConfigCliURL" -}}
{{- if .Values.keycloakConfigCli.url -}}
{{- .Values.keycloakConfigCli.url -}}
{{- else -}}
http://{{ .Values.keycloakx.fullnameOverride | default (printf "%s-keycloakx" .Release.Name) }}-http:80
{{- end -}}
{{- end }}

{{/* Realm name = the last path segment of an issuer URL. */}}
{{- define "proxcloud.realmName" -}}
{{- splitList "/" . | last -}}
{{- end }}

{{/* RabbitmqCluster name; the operator names its Service after it and its Secret <name>-default-user. */}}
{{- define "proxcloud.rabbitClusterName" -}}
{{- .Values.rabbitmqCluster.name | default (printf "%s-rabbitmq" (include "proxcloud.fullname" .)) -}}
{{- end }}

{{/* RabbitMQ host: RabbitmqCluster, else bundled subchart service, else the external host. */}}
{{- define "proxcloud.rabbitHost" -}}
{{- if .Values.rabbitmqCluster.enabled -}}
{{- include "proxcloud.rabbitClusterName" . -}}
{{- else if .Values.rabbitmq.enabled -}}
{{- /* honor the subchart's fullnameOverride — that IS its service name */ -}}
{{- .Values.rabbitmq.fullnameOverride | default (printf "%s-rabbitmq" .Release.Name) -}}
{{- else -}}
{{- required "externalRabbitmq.host is required when rabbitmq.enabled=false" .Values.externalRabbitmq.host -}}
{{- end -}}
{{- end }}
