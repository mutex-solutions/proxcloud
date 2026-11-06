{{/*
Shared backup helpers. The values contract carries ONE canonical shape
(backup.enabled / schedule / retentionDays / runAt / s3.*); each engine's
template translates it into whatever its operator wants, and the differences
between them are exactly why this lives in one place.

THE CRON ARITY TRAP: proxcloud stores a plain 5-field cron. CloudNativePG's
ScheduledBackup wants SIX fields (leading seconds) and silently misreads a
5-field string as seconds-minutes-hour-dom-month — i.e. it runs at the wrong
time rather than failing. `database-cluster.backupCron6` prepends the seconds
field; Percona and mariadb take the 5-field string as-is.
*/}}

{{- define "database-cluster.backupEnabled" -}}
{{- if and .Values.backup.enabled .Values.backup.s3.bucket .Values.backup.s3.endpoint -}}true{{- end -}}
{{- end }}

{{/* 5-field cron -> 6-field (CNPG). Anything already 6-field passes through. */}}
{{- define "database-cluster.backupCron6" -}}
{{- $c := .Values.backup.schedule | trim -}}
{{- if eq (len (splitList " " $c)) 5 -}}
{{- printf "0 %s" $c -}}
{{- else -}}
{{- $c -}}
{{- end -}}
{{- end }}

{{/* s3://bucket/prefix/<id> — one folder per database so retention and delete
     never reach a sibling's data. */}}
{{- define "database-cluster.backupPath" -}}
{{- $name := include "database-cluster.name" . -}}
{{- with .Values.backup.s3.prefix -}}
{{- printf "s3://%s/%s/%s" $.Values.backup.s3.bucket . $name -}}
{{- else -}}
{{- printf "s3://%s/%s" .Values.backup.s3.bucket $name -}}
{{- end -}}
{{- end }}

{{/* The SOURCE database's folder, for recovery. Derived exactly like
     backupPath so the two sides can never disagree about where a backup went. */}}
{{- define "database-cluster.restorePath" -}}
{{- with .Values.restore.s3.prefix -}}
{{- printf "s3://%s/%s/%s" $.Values.restore.s3.bucket . $.Values.restore.sourceId -}}
{{- else -}}
{{- printf "s3://%s/%s" .Values.restore.s3.bucket .Values.restore.sourceId -}}
{{- end -}}
{{- end }}

{{- define "database-cluster.restoreHostPort" -}}
{{- .Values.restore.s3.endpoint | trimPrefix "https://" | trimPrefix "http://" | trimSuffix "/" -}}
{{- end }}

{{- define "database-cluster.restoreTLS" -}}
{{- if hasPrefix "https://" .Values.restore.s3.endpoint -}}true{{- else -}}false{{- end -}}
{{- end }}

{{/* mariadb wants HOST:PORT with NO scheme and decides TLS from a separate
     flag, so the scheme has to be stripped and remembered. */}}
{{- define "database-cluster.backupHostPort" -}}
{{- .Values.backup.s3.endpoint | trimPrefix "https://" | trimPrefix "http://" | trimSuffix "/" -}}
{{- end }}

{{- define "database-cluster.backupTLS" -}}
{{- if hasPrefix "https://" .Values.backup.s3.endpoint -}}true{{- else -}}false{{- end -}}
{{- end }}

{{/* Retention for barman, which takes a barman retention string ("30d"). */}}
{{- define "database-cluster.backupRetention" -}}
{{- with .Values.backup.retentionDays -}}
{{- printf "%dd" (int .) -}}
{{- end -}}
{{- end }}

{{/* Retention for mariadb-operator, whose maxRetention is a GO DURATION parsed
     by time.ParseDuration — which has NO day unit. "30d" is rejected outright
     ("unknown unit \"d\" in duration"), so days become hours. */}}
{{- define "database-cluster.backupRetentionHours" -}}
{{- with .Values.backup.retentionDays -}}
{{- printf "%dh" (mul (int .) 24) -}}
{{- end -}}
{{- end }}
