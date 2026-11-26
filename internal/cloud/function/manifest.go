package function

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// Deployment coordinates. Deployments go through kamajik8s' generic group/version/plural helpers.
const (
	depGroup   = "apps"
	depVersion = "v1"
	depPlural  = "deployments"
)

// DrainSeconds is how long an instance that was told to stop keeps serving before its process
// gets SIGTERM. sandbox-proxy caches a function's ready pods for 2 s, so for that long it still sends
// requests to a pod that a rollout, a scale-down or a delete has just stopped; an image that
// exits at once on SIGTERM (most small servers do) would refuse them. The wait is a preStop sleep
// run by the kubelet, so it needs no shell in the image. It must stay longer than the proxy's
// cache and is part of terminationGracePeriodSeconds.
const DrainSeconds = 3

// EphemeralStorage is what one instance may write to its own filesystem and logs, as request and as limit: past it
// the kubelet evicts the pod, before the machine's disk fills for every other tenant on it. One size for every
// function.
// ponytail: a constant; a field of the snapshot beside cpuMillis and memoryMi when a function needs more.
const EphemeralStorage = "1Gi"

// Function is a function's identity and routing state, as rendered onto its Deployment. Where its pods run is not
// here and not the provider's to say per function: the RuntimeClass schedules them onto the sandbox machines.
type Function struct {
	ID        string
	ProjectID string
	Host      string // <name>-<6 hex>.<domain>
	Suspended bool
}

// ownerLabels are on everything proxcloud creates for a project's functions.
func ownerLabels(serviceID, projectID string) map[string]any {
	return map[string]any{
		sandbox.LabelProject:   projectID,
		sandbox.LabelService:   serviceID,
		sandbox.LabelManagedBy: sandbox.ManagedByValue,
	}
}

// BuildQuota renders the ResourceQuota of a project's function namespace: the hard backstop behind the API's own
// checks, with the project's three numbers. Every pod requests what it is limited to (BuildDeployment), so the
// requests are what the instances hold. The pod count leaves each function one more than the most instances a function
// may have, for the pod a rollout surges; CPU and memory leave none: a rollout of a project at its limit is refused by
// this quota, and the instances that serve keep serving.
func BuildQuota(cfg sandbox.Config, serviceID, projectID string, q sandbox.FunctionQuota) map[string]any {
	fc := cfg.WithDefaults().Functions
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ResourceQuota",
		"metadata": map[string]any{
			"name":      QuotaName,
			"namespace": NamespaceFor(projectID),
			"labels":    ownerLabels(serviceID, projectID),
		},
		"spec": map[string]any{
			"hard": map[string]any{
				"count/deployments.apps": strconv.Itoa(q.Functions),
				"requests.cpu":           strconv.Itoa(q.VCPUs),
				"requests.memory":        fmt.Sprintf("%dMi", q.RAMMb),
				"pods":                   strconv.Itoa(q.Functions * (fc.MaxInstances + 1)),
			},
		},
	}
}

// BuildDeployment renders a function's Deployment for one deployment snapshot.
//
// It has NO spec.replicas, on purpose: kamajik8s applies with force=true, so a replicas field
// would reset the scale on every apply. A new Deployment starts at the API default of 1; after
// that the scale changes only through the scale subresource (Service.SetScale).
//
// Security posture (customer code is untrusted), as for sandboxes and held by the cluster's pod policy whoever
// creates the pod: the provider's gVisor runtime class, no service account token, no service links, and no volume
// of any kind (a volume is how a Secret, a projected token or a host path would get into the pod). Egress and ingress
// are fenced by the namespace's labels; the egress rate is capped by the pod's bandwidth annotation.
//
// The image is pulled on every start (imagePullPolicy Always), whether it is named by tag or by digest: each start
// asks the registry, with the function's own credentials or none. A machine that has a private image in its cache
// would otherwise start it for any function that names it, another project's too, and by digest a name is all it
// takes; and a rollout would prove nothing of credentials that were replaced or removed.
//
// Requests equal limits, for CPU, memory and the instance's own disk: what an instance may use is what the scheduler
// set aside for it and what the namespace's quota counts.
func BuildDeployment(cfg sandbox.Config, serviceID string, f Function, snap Snapshot) map[string]any {
	cfg = cfg.WithDefaults()
	podLabels := ownerLabels(serviceID, f.ProjectID)
	podLabels[LabelID] = f.ID
	labels := ownerLabels(serviceID, f.ProjectID)
	labels[LabelID] = f.ID
	labels[LabelHost] = HostLabel(f.Host)
	annotations := map[string]any{AnnotationPort: strconv.Itoa(snap.Port)}
	if f.Suspended {
		annotations[AnnotationSuspended] = "true"
	}

	// ponytail: user env goes inline in the pod spec like sandbox env does today; what must not
	// be readable there is a secret (envFrom below).
	env := []any{map[string]any{"name": "PORT", "value": strconv.Itoa(snap.Port)}}
	keys := make([]string, 0, len(snap.Env))
	for k := range snap.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, map[string]any{"name": k, "value": literal(snap.Env[k])})
	}
	resources := func() map[string]any {
		return map[string]any{"cpu": fmt.Sprintf("%dm", snap.CPUMillis), "memory": fmt.Sprintf("%dMi", snap.MemoryMi), "ephemeral-storage": EphemeralStorage}
	}
	container := map[string]any{
		"name":            "main",
		"image":           snap.Image,
		"imagePullPolicy": "Always",
		"env":             env,
		"resources":       map[string]any{"limits": resources(), "requests": resources()},
		// A network probe is fine here (unlike sandboxes): the kubelet passes the ingress policy.
		"readinessProbe": map[string]any{"tcpSocket": map[string]any{"port": snap.Port}, "periodSeconds": 1},
		"lifecycle":      map[string]any{"preStop": map[string]any{"sleep": map[string]any{"seconds": DrainSeconds}}},
	}
	// Secrets reach the container from the function's Secret (BuildSecret) as environment, never through this
	// manifest and never as a volume. A function without secrets has neither field.
	podAnnotations := map[string]any{
		AnnotationPort:                   strconv.Itoa(snap.Port),
		"kubernetes.io/egress-bandwidth": fmt.Sprintf("%dM", cfg.EgressMbps),
	}
	if snap.SecretsVersion != "" {
		container["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": f.ID}}}
		podAnnotations[AnnotationSecrets] = snap.SecretsVersion
	}
	// Unset command/args leave the image's own ENTRYPOINT/CMD in charge.
	if len(snap.Command) > 0 {
		container["command"] = literals(snap.Command)
	}
	if len(snap.Args) > 0 {
		container["args"] = literals(snap.Args)
	}
	podSpec := map[string]any{
		"runtimeClassName":              cfg.RuntimeClass,
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"terminationGracePeriodSeconds": 10 + DrainSeconds, // the process keeps its 10 s after SIGTERM
		"containers":                    []any{container},
	}
	// The function's own pull secret (BuildPullSecret) and no other: the platform lends a function no registry
	// account of its own.
	if snap.RegistryAuth != nil {
		podSpec["imagePullSecrets"] = []any{map[string]any{"name": PullSecretName(f.ID)}}
		podAnnotations[AnnotationRegistryAuth] = snap.RegistryAuth.Version
	}
	return map[string]any{
		"apiVersion": depGroup + "/" + depVersion,
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":        f.ID,
			"namespace":   NamespaceFor(f.ProjectID),
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": map[string]any{
			"selector":                map[string]any{"matchLabels": map[string]any{LabelID: f.ID}},
			"progressDeadlineSeconds": 120,
			"strategy":                map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": 1, "maxUnavailable": 0}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": podLabels, "annotations": podAnnotations},
				"spec":     podSpec,
			},
		},
	}
}

// BuildSecret renders the Secret that carries a function's secret values to its instances: named
// like the Deployment, every name one key. ownerUID is the uid of the function's Deployment: the
// Secret is garbage-collected with it (proxcloud may create and patch Secrets, not read or delete them).
// Empty while the Deployment does not exist yet.
//
// The values are in data, base64, and never in stringData: an apply owns exactly the keys it names, so a name that
// is no longer among the secrets leaves the Secret with the apply that drops it.
func BuildSecret(serviceID string, f Function, ownerUID string, secrets map[string]string) map[string]any {
	labels := ownerLabels(serviceID, f.ProjectID)
	labels[LabelID] = f.ID
	meta := map[string]any{"name": f.ID, "namespace": NamespaceFor(f.ProjectID), "labels": labels}
	if ownerUID != "" {
		meta["ownerReferences"] = []any{map[string]any{
			"apiVersion": depGroup + "/" + depVersion, "kind": "Deployment", "name": f.ID, "uid": ownerUID,
		}}
	}
	data := make(map[string]any, len(secrets))
	for k, v := range secrets {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": meta, "data": data}
}

// PullSecretName names the Secret that carries a function's registry credentials. Not the
// function's Secret: an image pull secret has its own type, and a Secret's type cannot change.
func PullSecretName(id string) string { return id + "-registry" }

// BuildPullSecret renders the function's registry credentials as an image pull secret, owned like
// BuildSecret's. It is the only object the password is ever written to.
func BuildPullSecret(serviceID string, f Function, ownerUID string, auth RegistryAuth, password string) map[string]any {
	cfg, _ := json.Marshal(map[string]any{"auths": map[string]any{auth.Server: map[string]string{"username": auth.Username, "password": password}}})
	sec := BuildSecret(serviceID, f, ownerUID, map[string]string{".dockerconfigjson": string(cfg)})
	sec["type"] = "kubernetes.io/dockerconfigjson"
	sec["metadata"].(map[string]any)["name"] = PullSecretName(f.ID)
	return sec
}

// literal escapes a value of the customer's for the container's command, args and env values,
// where the kubelet replaces $(NAME) by the variable of that name and $$ by $: with every $
// doubled the container gets the text as it was written. Only in the rendered manifest: records and
// deployment snapshots keep what the customer sent. Secret values need none of this, the kubelet
// does not expand what comes from a Secret (envFrom).
func literal(s string) string { return strings.ReplaceAll(s, "$", "$$") }

func literals(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = literal(s)
	}
	return out
}
