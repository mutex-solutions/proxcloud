{{/*
Base name for every object this chart renders: the proxcloud resource id
(pc-d-xxxxxxxx), NOT the release name. internal/cloud/dbaas derives secret and
service names from the resource id (ConnectionSecret tuples, <id>-lb), so the
release name must stay an ArgoCD implementation detail.
*/}}
{{- define "database-cluster.name" -}}
{{- required "proxcloud.resourceId is required (stamped by internal/cloud/dbaas/values.go BuildValues)" .Values.proxcloud.resourceId -}}
{{- end }}

{{/*
Fail-fast contract validation. Included from service-lb.yaml (which is
evaluated for every engine — for kafka the Service body is skipped but this
include still runs), so a values document missing required keys can never
produce a half-formed database. Mirrors the Validate() checks in
internal/cloud/dbaas — this side is the last line of defence, not the UX.
*/}}
{{- define "database-cluster.validate" -}}
{{- $engines := list "postgresql" "mysql" "mariadb" "valkey" "ferretdb" "opensearch" "kafka" -}}
{{- if not .Values.engine -}}
{{- fail "engine is required (postgresql|mysql|mariadb|valkey|ferretdb|opensearch|kafka)" -}}
{{- end -}}
{{- if not (has .Values.engine $engines) -}}
{{- fail (printf "unknown engine %q (want postgresql|mysql|mariadb|valkey|ferretdb|opensearch|kafka)" .Values.engine) -}}
{{- end -}}
{{- if not .Values.engineVersion -}}
{{- fail "engineVersion is required" -}}
{{- end -}}
{{- if not .Values.instances -}}
{{- fail "instances is required (>= 1)" -}}
{{- end -}}
{{- if not .Values.resources.cpu -}}
{{- fail "resources.cpu is required" -}}
{{- end -}}
{{- if not .Values.resources.memoryGi -}}
{{- fail "resources.memoryGi is required" -}}
{{- end -}}
{{- if and (ne .Values.engine "ferretdb") (not .Values.storage.sizeGi) -}}
{{- fail "storage.sizeGi is required" -}}
{{- end -}}
{{- if not .Values.proxcloud.projectId -}}
{{- fail "proxcloud.projectId is required" -}}
{{- end -}}
{{- if not .Values.proxcloud.resourceId -}}
{{- fail "proxcloud.resourceId is required" -}}
{{- end -}}
{{- end }}

{{/*
Common labels. app.kubernetes.io/managed-by is the proxcloud ownership marker the
sync provider filters on; proxcloud.io/project ties every object back to the
owning project. app.kubernetes.io/instance doubles as the NetworkPolicy pod
selector — the engine templates stamp it onto their pods (CNPG via
inheritedMetadata; the operators propagate the CR name as instance by
convention).
*/}}
{{- define "database-cluster.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/managed-by: proxcloud
app.kubernetes.io/instance: {{ include "database-cluster.name" . }}
proxcloud.io/project: {{ required "proxcloud.projectId is required" .Values.proxcloud.projectId | quote }}
{{- end }}

{{/*
Common annotations. Display name is an annotation, not a label — it is
free-form user input and label values have a restricted charset.
*/}}
{{- define "database-cluster.annotations" -}}
{{- with .Values.proxcloud.displayName }}
proxcloud.io/display-name: {{ . | quote }}
{{- end }}
{{- end }}

{{/*
MetalLB annotations for a tenant-facing LoadBalancer Service. Every LB the
chart (or Strimzi, via the listener template) renders carries the provider's
address pool, so the endpoint always lands in the services subnet tenants reach
over their normal egress. Empty pool = no annotation (MetalLB then picks from
any auto-assign pool). Callers indent: `include ... | nindent N`.
*/}}
{{- define "database-cluster.lbAnnotations" -}}
{{- with .Values.network.addressPool }}
metallb.universe.tf/address-pool: {{ . | quote }}
{{- end }}
{{- end }}

{{/*
Engine -> client port. The single port the LB Service exposes (for kafka —
whose LB Services are Strimzi-owned, not chart-owned — this is the external
listener port, used by the NetworkPolicy).
*/}}
{{- define "database-cluster.port" -}}
{{- $ports := dict "postgresql" 5432 "mysql" 3306 "mariadb" 3306 "valkey" 6379 "ferretdb" 27017 "opensearch" 9200 "kafka" 9094 -}}
{{- index $ports .Values.engine -}}
{{- end }}

{{/*
engineVersion -> image, per engine. Ops-updatable per chart release: bump a
tag here -> bump Chart.yaml version -> re-pin providers. A version with no
mapping fails the render — better a loud sync error than a silently wrong
major. Every tag below was verified to exist against its registry's tags API
on 2026-08-03 (Docker Hub / ghcr), and the mysql tags are exactly the builds
Percona documents as tested with ps-operator 1.2.0.

opensearch/kafka have NO map here BY DESIGN: both operators resolve images
from the version field natively (OpenSearchCluster spec.general.version,
Kafka spec.kafka.version), so their templates never call this helper — the
guard below turns an accidental future call into a loud render failure.

valkey is 9.x ONLY. valkey-operator v0.4.0 writes `shutdown-on-sigterm failover`
into every server config unconditionally (internal/controller/config.go), and
its own docs say that directive "requires Valkey 9.0+". On anything older the
server refuses the config file outright —

  *** FATAL CONFIG FILE ERROR *** ... 'shutdown-on-sigterm failover'
  argument(s) must be one of the following: default, save, nosave, now, force

— and CrashLoopBackOffs forever while the operator sits at Reconciling. 7.2,
8.0 and 8.1 were each verified dead against this operator on the 2026-08-06
drill: they are not older versions we still offer, they are versions that
cannot start. Dropping them turns a create into a render-time failure instead
of a database that never boots.
*/}}
{{- define "database-cluster.image" -}}
{{- if has .Values.engine (list "opensearch" "kafka") -}}
{{- fail (printf "database-cluster.image must not be called for %s — its operator resolves the image from engineVersion natively" .Values.engine) -}}
{{- end -}}
{{- $v := .Values.engineVersion | toString -}}
{{- $maps := dict
      "postgresql" (dict
        "14" "ghcr.io/cloudnative-pg/postgresql:14.23"
        "15" "ghcr.io/cloudnative-pg/postgresql:15.18"
        "16" "ghcr.io/cloudnative-pg/postgresql:16.14"
        "17" "ghcr.io/cloudnative-pg/postgresql:17.10"
        "18" "ghcr.io/cloudnative-pg/postgresql:18.4")
      "mysql" (dict
        "8.0" "docker.io/percona/percona-server:8.0.46-37"
        "8.4" "docker.io/percona/percona-server:8.4.10-10")
      "mariadb" (dict
        "10.6" "docker.io/library/mariadb:10.6.27"
        "10.11" "docker.io/library/mariadb:10.11.18"
        "11.4" "docker.io/library/mariadb:11.4.12"
        "11.8" "docker.io/library/mariadb:11.8.8")
      "valkey" (dict
        "9.0" "docker.io/valkey/valkey:9.0.5"
        "9.1" "docker.io/valkey/valkey:9.1.1")
      "ferretdb" (dict
        "2.5" "ghcr.io/ferretdb/ferretdb:2.5.0"
        "2.7" "ghcr.io/ferretdb/ferretdb:2.7.0")
-}}
{{- $m := index $maps .Values.engine -}}
{{- $img := index $m $v -}}
{{- if not $img -}}
{{- fail (printf "no image mapping for %s %q — extend database-cluster.image in _helpers.tpl (and release a new chart version)" .Values.engine $v) -}}
{{- end -}}
{{- $img -}}
{{- end }}

{{/*
mysql BACKUP image (xtrabackup). The Percona operator REFUSES a CR whose backup block carries no
image — CheckNSetDefaults fails with "backup.image can't be empty" and the WHOLE reconcile stops:
no pods, no <id>-secrets, no status, while ArgoCD reports the applied manifests healthy. The chart
wires a backup storage into every mysql database, so this is never optional.

The major must track the server (xtrabackup 8.0 cannot back up 8.4). Ops-updatable exactly like
the engine image map: bump a tag here, bump Chart.yaml, re-pin the provider.
*/}}
{{- define "database-cluster.mysqlBackupImage" -}}
{{- $v := .Values.engineVersion | toString -}}
{{- $map := dict
      "8.0" "docker.io/percona/percona-xtrabackup:8.0.35-36.1"
      "8.4" "docker.io/percona/percona-xtrabackup:8.4.0-6.1"
-}}
{{- $img := index $map $v -}}
{{- if not $img -}}
{{- fail (printf "no xtrabackup image for mysql %q — extend database-cluster.mysqlBackupImage in _helpers.tpl (and release a new chart version)" $v) -}}
{{- end -}}
{{- $img -}}
{{- end }}

{{/*
FerretDB BACKEND image (the DocumentDB-flavoured postgres CNPG runs). It is a MATCHED PAIR
with the frontend — a frontend on 2.7 against a 2.5 backend is unsupported — so it is mapped
off the SAME engineVersion instead of being pinned in values. That pairing is what makes
UPGRADE safe for ferretdb: one version bump moves both halves together.
An explicit values.ferretdb.postgresImage still wins, for pinning during an incident.
*/}}
{{- define "database-cluster.ferretdbPostgresImage" -}}
{{- if .Values.ferretdb.postgresImage -}}
{{- .Values.ferretdb.postgresImage -}}
{{- else -}}
{{- $v := .Values.engineVersion | toString -}}
{{- $pairs := dict
      "2.5" "ghcr.io/ferretdb/postgres-documentdb:17-0.106.0-ferretdb-2.5.0"
      "2.7" "ghcr.io/ferretdb/postgres-documentdb:17-0.107.0-ferretdb-2.7.0"
-}}
{{- $img := index $pairs $v -}}
{{- if not $img -}}
{{- fail (printf "no backend image pair for ferretdb %q — extend database-cluster.ferretdbPostgresImage in _helpers.tpl (and release a new chart version)" $v) -}}
{{- end -}}
{{- $img -}}
{{- end -}}
{{- end }}

{{/*
FerretDB frontend image: explicit override wins, else the engineVersion map.
*/}}
{{- define "database-cluster.ferretdbImage" -}}
{{- if .Values.ferretdb.image -}}
{{- .Values.ferretdb.image -}}
{{- else -}}
{{- include "database-cluster.image" . -}}
{{- end -}}
{{- end }}

{{/*
Guaranteed-QoS resources block (requests == limits): the customer pays for the
full size, so the full size is reserved.
*/}}
{{- define "database-cluster.resources" -}}
requests:
  cpu: {{ .Values.resources.cpu | quote }}
  memory: {{ .Values.resources.memoryGi }}Gi
limits:
  cpu: {{ .Values.resources.cpu | quote }}
  memory: {{ .Values.resources.memoryGi }}Gi
{{- end }}

{{/*
Pod placement, in the plain Kubernetes shape (nodeSelector + tolerations). Emits nothing when
the provider configures neither, so an unconfigured database keeps rendering byte-identically.
Every engine template includes this at whatever path its CRD puts pod scheduling under.
*/}}
{{- define "database-cluster.podPlacement" -}}
{{- $p := dict -}}
{{- with .Values.scheduling.nodeSelector }}{{- $_ := set $p "nodeSelector" . }}{{- end -}}
{{- with .Values.scheduling.tolerations }}{{- $_ := set $p "tolerations" . }}{{- end -}}
{{- with $p }}{{ toYaml . }}{{- end -}}
{{- end }}

{{/*
Hard anti-affinity across the failure-domain key (scheduling.zoneKey, default
topology.kubernetes.io/zone): no two pods matching `labels` may share a zone.
`required`, not `preferred` — a replica set that silently lands in one zone is
not highly available, so with fewer zones than instances the surplus pod stays
Pending instead. A site with a single failure domain overrides
scheduling.zoneKey (e.g. kubernetes.io/hostname). Callers gate it on
instances > 1 and pass (dict "root" . "labels" <pod label map of ONE database>).
*/}}
{{- define "database-cluster.zoneAntiAffinity" -}}
podAntiAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
    - topologyKey: {{ .root.Values.scheduling.zoneKey | quote }}
      labelSelector:
        matchLabels:
          {{- toYaml .labels | nindent 10 }}
{{- end }}

{{/*
The same placement in Strimzi's shape. Strimzi's PodTemplate has tolerations but NO
nodeSelector, so the selector has to be expressed as a required nodeAffinity — one matchExpression
per label, all of which must match (In with a single value == equality), which is precisely what a
nodeSelector means. `spread` adds the zone anti-affinity (node pool only; the entity operator
is a single pod). Emits nothing when nothing is configured.
Called with (dict "root" . "spread" <bool>).
*/}}
{{- define "database-cluster.strimziPodTemplate" -}}
{{- $s := .root.Values.scheduling -}}
{{- $name := include "database-cluster.name" .root -}}
{{- $spread := and .spread (gt (int .root.Values.instances) 1) -}}
{{- if or $s.nodeSelector $s.tolerations $spread }}
template:
  pod:
    {{- if or $s.nodeSelector $spread }}
    affinity:
      {{- with $s.nodeSelector }}
      nodeAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
          nodeSelectorTerms:
            - matchExpressions:
                {{- range $k, $v := . }}
                - key: {{ $k }}
                  operator: In
                  values:
                    - {{ $v | quote }}
                {{- end }}
      {{- end }}
      {{- if $spread }}
      {{- include "database-cluster.zoneAntiAffinity" (dict "root" .root "labels" (dict "strimzi.io/cluster" $name "strimzi.io/pool-name" (printf "%s-pool" $name))) | nindent 6 }}
      {{- end }}
    {{- end }}
    {{- with $s.tolerations }}
    tolerations: {{ toYaml . | nindent 6 }}
    {{- end }}
{{- end }}
{{- end }}

{{/*
Per-engine pod selector for the LB Service: the WRITE endpoint of each engine.
  postgresql: the CNPG primary. TODO-verify(drill): cnpg.io/instanceRole is the
              current label on CNPG >= 1.25 (older docs show `role`); if 1.29
              drops it, fall back to CNPG managed.services.additional.
  mysql:      the Percona operator's HAProxy pods (routes writes to the GR
              primary). TODO-verify(drill): label pair on the pinned operator.
  mariadb:    the CHART-OWNED HAProxy deployment (templates/mariadb/) — the
              operator's <id>-primary Service is pod-name-pinned, not
              selectable.
  valkey:     best-known valkey-operator labels. TODO-verify(beta): unverified
              until the valkey CRD is pinned — see templates/valkey/valkey.yaml.
  ferretdb:   the chart's own frontend Deployment (labels stamped by us).
  opensearch: the operator's node-pool pods (any node serves reads/writes on
              9200 — no primary/replica split to select). Label pair verified
              on reference manifests; TODO-verify(drill): operator 3.x is
              migrating the api group opster.io -> opensearch.org, re-check
              the pod labels on 3.0.2.
  kafka:      NOT USED — service-lb.yaml skips kafka entirely; Strimzi owns
              the external bootstrap + per-broker LB Services (see
              LBServiceNameFor in internal/cloud/dbaas/engines.go).
*/}}
{{/*
Read-only port on the SHARED proxy. mysql (Percona haproxy) and mariadb (the
chart-owned haproxy) both route reads on 3307, so their read endpoint is a
second PORT on the existing load balancer rather than a second load balancer —
no extra load balancer, no extra cost. postgresql cannot do this: its
readers are different pods, so it gets its own LB (postgresql/service-ro-lb.yaml).
Empty = this engine has no shared-proxy read port.
*/}}
{{- define "database-cluster.readPort" -}}
{{- if has .Values.engine (list "mysql" "mariadb") -}}3307{{- end -}}
{{- end }}

{{- define "database-cluster.serviceSelector" -}}
{{- $name := include "database-cluster.name" . -}}
{{- if eq .Values.engine "postgresql" -}}
cnpg.io/cluster: {{ $name }}
cnpg.io/instanceRole: primary
{{- else if eq .Values.engine "mysql" -}}
app.kubernetes.io/instance: {{ $name }}
app.kubernetes.io/component: haproxy
{{- else if eq .Values.engine "mariadb" -}}
app.kubernetes.io/name: mariadb-haproxy
app.kubernetes.io/instance: {{ $name }}
{{- else if eq .Values.engine "valkey" -}}
{{- /* valkey.io/cluster, the operator's own cluster label — NOT
     app.kubernetes.io/instance, which valkey-operator stamps PER NODE as
     `<cluster>-<shard>-<node>` (e.g. pc-d-abc12345-0-0). Selecting on the bare
     cluster id matched no pod, so this Service had zero endpoints and the
     LB fronted nothing. */}}
valkey.io/cluster: {{ $name }}
{{- else if eq .Values.engine "ferretdb" -}}
app.kubernetes.io/name: ferretdb
app.kubernetes.io/instance: {{ $name }}
{{- else if eq .Values.engine "opensearch" -}}
{{/* opensearch.org/*, NOT opster.io/*. The labels DID move with the api group on
     operator 3.0.2 — drill-verified off a live node pod, which carries
     opensearch.org/opensearch-cluster + opensearch.org/opensearch-nodepool and no
     opster.io key at all. The bootstrap pod carries only the cluster label, so
     selecting on the nodepool too still keeps it out of the LB. */}}
opensearch.org/opensearch-cluster: {{ $name }}
opensearch.org/opensearch-nodepool: nodes
{{- end -}}
{{- end }}
