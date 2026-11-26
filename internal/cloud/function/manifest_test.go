package function

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

func testConfig() sandbox.Config {
	return sandbox.Config{Domain: "sbx.example.com", Functions: sandbox.FunctionsConfig{Enabled: true}}
}

func TestBuildDeployment(t *testing.T) {
	f := Function{ID: "fn-0123456789abcdef", ProjectID: "p1", Host: "api-a1b2c3.sbx.example.com"}
	snap := Snapshot{Image: "ghcr.io/acme/api:1", Port: 3000, CPUMillis: 250, MemoryMi: 512, Env: map[string]string{"B": "2", "A": "1"}}
	dep := BuildDeployment(testConfig(), "svc-1", f, snap)

	// The applied manifest must never carry a replica count (force-apply would reset the scale).
	raw, _ := json.Marshal(dep)
	if strings.Contains(string(raw), "replicas") {
		t.Fatalf("manifest carries replicas: %s", raw)
	}
	if dep["apiVersion"] != "apps/v1" || dep["kind"] != "Deployment" || digStr(dep, "metadata", "name") != f.ID || digStr(dep, "metadata", "namespace") != "fn-p1" {
		t.Fatalf("identity: %s", raw)
	}
	wantPodLabels := map[string]any{
		"app.kubernetes.io/managed-by": "proxcloud", "proxcloud.io/project": "p1", "proxcloud.io/service": "svc-1",
		"proxcloud.io/function-id": f.ID,
	}
	if got := dig(dep, "spec", "template", "metadata", "labels"); !reflect.DeepEqual(got, wantPodLabels) {
		t.Errorf("pod labels: %v", got)
	}
	wantLabels := map[string]any{"proxcloud.io/function-host": "api-a1b2c3"}
	for k, v := range wantPodLabels {
		wantLabels[k] = v
	}
	if got := dig(dep, "metadata", "labels"); !reflect.DeepEqual(got, wantLabels) {
		t.Errorf("deployment labels: %v", got)
	}
	// The port is routing metadata on the Deployment and on each pod (a rollout that changes the
	// port has pods on both). The pod carries its egress cap beside it, the provider's.
	if got := dig(dep, "metadata", "annotations"); !reflect.DeepEqual(got, map[string]any{"proxcloud.io/function-port": "3000"}) {
		t.Errorf("deployment annotations: %v", got)
	}
	if got := dig(dep, "spec", "template", "metadata", "annotations"); !reflect.DeepEqual(got, map[string]any{"proxcloud.io/function-port": "3000", "kubernetes.io/egress-bandwidth": "100M"}) {
		t.Errorf("pod annotations: %v", got)
	}
	if got := dig(dep, "spec", "selector", "matchLabels"); !reflect.DeepEqual(got, map[string]any{"proxcloud.io/function-id": f.ID}) {
		t.Errorf("selector: %v", got)
	}
	if dig(dep, "spec", "progressDeadlineSeconds") != 120 || !reflect.DeepEqual(dig(dep, "spec", "strategy"), map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": 1, "maxUnavailable": 0}}) {
		t.Errorf("rollout settings: %v", dep["spec"])
	}

	pod, _ := dig(dep, "spec", "template", "spec").(map[string]any)
	if pod["runtimeClassName"] != "gvisor" || pod["automountServiceAccountToken"] != false || pod["enableServiceLinks"] != false || pod["terminationGracePeriodSeconds"] != 13 {
		t.Errorf("pod posture: %v", pod)
	}
	for _, k := range []string{"tolerations", "nodeSelector", "imagePullSecrets"} {
		if pod[k] != nil {
			t.Errorf("pod has %s: %v", k, pod[k])
		}
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	if c["name"] != "main" || c["image"] != snap.Image || c["command"] != nil || c["args"] != nil || c["envFrom"] != nil {
		t.Errorf("container: %v", c)
	}
	wantEnv := []any{
		map[string]any{"name": "PORT", "value": "3000"},
		map[string]any{"name": "A", "value": "1"},
		map[string]any{"name": "B", "value": "2"},
	}
	if !reflect.DeepEqual(c["env"], wantEnv) {
		t.Errorf("env: %v", c["env"])
	}
	// Requests equal limits: what the instance may use is what the scheduler set aside and what the namespace's
	// quota counts (requests.cpu, requests.memory). Its own disk is 1 GiB, as request and as limit.
	size := map[string]any{"cpu": "250m", "memory": "512Mi", "ephemeral-storage": "1Gi"}
	if !reflect.DeepEqual(c["resources"], map[string]any{"limits": size, "requests": size}) {
		t.Errorf("resources: %v", c["resources"])
	}
	if !reflect.DeepEqual(c["readinessProbe"], map[string]any{"tcpSocket": map[string]any{"port": 3000}, "periodSeconds": 1}) {
		t.Errorf("probe: %v", c["readinessProbe"])
	}
	// A stopped instance keeps serving for 3 s (the proxy still has its address), then gets
	// SIGTERM and its 10 s: the kubelet's sleep, so an image without a shell has it too.
	if !reflect.DeepEqual(c["lifecycle"], map[string]any{"preStop": map[string]any{"sleep": map[string]any{"seconds": 3}}}) {
		t.Errorf("lifecycle: %v", c["lifecycle"])
	}

	// Suspended, own command, a larger size, another egress cap of the provider.
	f.Suspended = true
	snap = Snapshot{Image: "ghcr.io/acme/api@sha256:abc", Port: 8080, CPUMillis: 2000, MemoryMi: 256, Command: []string{"node"}, Args: []string{"server.js"}}
	cfg := testConfig()
	cfg.EgressMbps = 25
	dep = BuildDeployment(cfg, "svc-1", f, snap)
	if digStr(dep, "metadata", "annotations", AnnotationSuspended) != "true" {
		t.Errorf("suspended annotation: %v", dig(dep, "metadata", "annotations"))
	}
	if got := digStr(dep, "spec", "template", "metadata", "annotations", "kubernetes.io/egress-bandwidth"); got != "25M" {
		t.Errorf("egress cap %q, want the provider's 25M", got)
	}
	pod, _ = dig(dep, "spec", "template", "spec").(map[string]any)
	c = pod["containers"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(c["command"], []any{"node"}) || !reflect.DeepEqual(c["args"], []any{"server.js"}) {
		t.Errorf("command/args: %v %v", c["command"], c["args"])
	}
	if digStr(c, "resources", "requests", "cpu") != "2000m" || digStr(c, "resources", "limits", "cpu") != "2000m" {
		t.Errorf("resources: %v", c["resources"])
	}
}

// What the cluster's pod policy holds a tenant pod to, a function's pod is rendered with, whatever the snapshot:
// gVisor, no token, no volume of any kind, no say in where it runs, nothing of the host. And the image is pulled on
// every start, by tag or by digest: a machine's cache never stands in for the registry's answer.
func TestPodHasNoVolumeNoTokenAndAlwaysPulls(t *testing.T) {
	f := Function{ID: "fn-0123456789abcdef", ProjectID: "p1", Host: "api-a1b2c3.sbx.example.com"}
	digest := "sha256:" + strings.Repeat("ab", 32)
	for name, snap := range map[string]Snapshot{
		"a tag":    {Image: "ghcr.io/acme/api:1", Port: 8080, CPUMillis: 250, MemoryMi: 256},
		"a digest": {Image: "ghcr.io/acme/api@" + digest, Port: 8080, CPUMillis: 250, MemoryMi: 256},
		// A rollback deploys the digest its revision ran with: another project that learns it must still be asked
		// for credentials by the registry.
		"a tag and a digest": {Image: "ghcr.io/acme/api:1@" + digest, Port: 8080, CPUMillis: 250, MemoryMi: 256},
		"no tag":             {Image: "nginx", Port: 8080, CPUMillis: 250, MemoryMi: 256},
		"everything a snapshot can carry": {Image: "ghcr.io/acme/api@" + digest, Port: 8080, CPUMillis: 2000, MemoryMi: 4096, Env: map[string]string{"A": "1"},
			Command: []string{"/bin/server"}, Args: []string{"--port", "8080"}, SecretNames: []string{"TOKEN"}, SecretsVersion: "v1",
			RegistryAuth: &RegistryAuth{Server: "ghcr.io", Username: "bot", Version: "v1"}},
	} {
		dep := BuildDeployment(testConfig(), "svc-1", f, snap)
		pod := dig(dep, "spec", "template", "spec").(map[string]any)
		containers := pod["containers"].([]any)
		c := containers[0].(map[string]any)
		if len(containers) != 1 || c["imagePullPolicy"] != "Always" {
			t.Errorf("%s: imagePullPolicy %v in %d containers, want Always in one", name, c["imagePullPolicy"], len(containers))
		}
		if pod["runtimeClassName"] != "gvisor" || pod["automountServiceAccountToken"] != false || pod["enableServiceLinks"] != false {
			t.Errorf("%s: pod posture: %v", name, pod)
		}
		// Only these keys: a new one is a decision about what a tenant's pod may have, made here in sight.
		allowedPod := map[string]bool{"runtimeClassName": true, "automountServiceAccountToken": true, "enableServiceLinks": true, "terminationGracePeriodSeconds": true, "containers": true, "imagePullSecrets": true}
		for k := range pod {
			if !allowedPod[k] {
				t.Errorf("%s: the pod spec has %q", name, k)
			}
		}
		allowedContainer := map[string]bool{"name": true, "image": true, "imagePullPolicy": true, "env": true, "envFrom": true, "resources": true, "readinessProbe": true, "lifecycle": true, "command": true, "args": true}
		for k := range c {
			if !allowedContainer[k] {
				t.Errorf("%s: the container has %q", name, k)
			}
		}
		raw, _ := json.Marshal(dep)
		for _, never := range []string{`"volumes"`, `"volumeMounts"`, `"volumeDevices"`, `"hostPath"`, `"hostNetwork"`, `"hostPID"`, `"hostIPC"`, `"hostPort"`, `"nodeName"`, `"nodeSelector"`, `"tolerations"`, `"securityContext"`, `"serviceAccountName"`, `"initContainers"`, `"ephemeralContainers"`, `"stringData"`,
			sandbox.LabelSandboxID, `"IfNotPresent"`, `"Never"`} {
			if strings.Contains(string(raw), never) {
				t.Errorf("%s: the manifest has %s: %s", name, never, raw)
			}
		}
		for _, r := range []string{"limits", "requests"} {
			if got := digStr(c, "resources", r, "ephemeral-storage"); got != "1Gi" {
				t.Errorf("%s: ephemeral-storage %s %q, want 1Gi", name, r, got)
			}
		}
		if !reflect.DeepEqual(dig(c, "resources", "limits"), dig(c, "resources", "requests")) {
			t.Errorf("%s: requests differ from limits: %v", name, c["resources"])
		}
		if got := digStr(dep, "spec", "template", "metadata", "annotations", "kubernetes.io/egress-bandwidth"); got != "100M" {
			t.Errorf("%s: egress cap %q", name, got)
		}
	}
}

// kubeletExpand is what the kubelet does to a container's command, args and env values before
// the container starts (k8s.io/kubernetes/third_party/forked/golang/expansion.Expand): $(NAME)
// becomes the variable of that name when there is one, $$ becomes $.
func kubeletExpand(in string, vars map[string]string) string {
	var out strings.Builder
	for i := 0; i < len(in); i++ {
		if in[i] != '$' || i+1 == len(in) {
			out.WriteByte(in[i])
			continue
		}
		switch rest := in[i+1:]; rest[0] {
		case '$':
			out.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(rest, ')')
			if end < 0 {
				out.WriteString("$(")
				i++
				break
			}
			if v, ok := vars[rest[1:end]]; ok {
				out.WriteString(v)
			} else {
				out.WriteString("$(" + rest[1:end] + ")")
			}
			i += end + 1
		default:
			out.WriteString("$" + rest[:1])
			i++
		}
	}
	return out.String()
}

// command, args and env values reach the container as the customer wrote them: what the kubelet
// would expand is escaped in the manifest, and only there (the snapshot is not touched). PORT is
// still set.
func TestBuildDeploymentLiteralDollars(t *testing.T) {
	// The reference itself: what the kubelet does to text that is not escaped.
	for in, want := range map[string]string{"$(HOME)": "/root", "$$": "$", "$$(X)": "$(X)", "$": "$", "$(NONE)": "$(NONE)", "a$b": "a$b"} {
		if got := kubeletExpand(in, map[string]string{"HOME": "/root", "X": "x"}); got != want {
			t.Fatalf("kubeletExpand(%q) = %q, want %q", in, got, want)
		}
	}
	texts := []string{"$(HOME)", "$$", "$$(X)", "$", "$(PORT)", "$(", "a$b$", "$$$(HOME)$", "echo $(date) && echo $$ > /tmp/pid", "plain"}
	env := map[string]string{"HOME": "/root", "X": "x"}
	for i, s := range texts {
		env["V"+string(rune('a'+i))] = s
	}
	snap := Snapshot{Image: "nginx:1", Port: 8080, CPUMillis: 250, MemoryMi: 256, Command: texts, Args: texts, Env: env}
	stored, _ := json.Marshal(snap)
	c := dig(BuildDeployment(testConfig(), "svc-1", Function{ID: "fn-1", ProjectID: "p1", Host: "a-b.sbx.example.com"}, snap), "spec", "template", "spec").(map[string]any)["containers"].([]any)[0].(map[string]any)
	if after, _ := json.Marshal(snap); string(after) != string(stored) {
		t.Errorf("rendering changed the snapshot: %s", after)
	}

	// The container's variables as the kubelet has them: every one of the manifest, expanded.
	vars := map[string]string{}
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		vars[m["name"].(string)] = kubeletExpand(m["value"].(string), vars)
	}
	if vars["PORT"] != "8080" || c["env"].([]any)[0].(map[string]any)["name"] != "PORT" {
		t.Errorf("PORT: %v", c["env"])
	}
	for k, want := range env {
		if vars[k] != want {
			t.Errorf("env %s reaches the container as %q, want %q", k, vars[k], want)
		}
	}
	for _, field := range []string{"command", "args"} {
		for i, v := range c[field].([]any) {
			if got := kubeletExpand(v.(string), vars); got != texts[i] {
				t.Errorf("%s %q reaches the container as %q", field, texts[i], got)
			}
		}
	}

	// A secret value is not expanded by the kubelet, so it is stored as it is.
	sec := BuildSecret("svc-1", Function{ID: "fn-1", ProjectID: "p1"}, "", map[string]string{"TOKEN": "$(HOME)$$"})
	if got, _ := base64.StdEncoding.DecodeString(digStr(sec, "data", "TOKEN")); string(got) != "$(HOME)$$" {
		t.Errorf("secret value: %q", got)
	}
}

// Secrets reach the container through the function's Secret: the Deployment names it and carries
// the version marker on the pod template (a change rolls out), never a value.
func TestBuildDeploymentSecrets(t *testing.T) {
	f := Function{ID: "fn-0123456789abcdef", ProjectID: "p1", Host: "api-a1b2c3.sbx.example.com"}
	snap := Snapshot{Image: "nginx:1", Port: 8080, CPUMillis: 250, MemoryMi: 256, SecretNames: []string{"TOKEN"}, SecretsVersion: "v1"}
	dep := BuildDeployment(testConfig(), "svc-1", f, snap)
	c := dig(dep, "spec", "template", "spec").(map[string]any)["containers"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(c["envFrom"], []any{map[string]any{"secretRef": map[string]any{"name": f.ID}}}) {
		t.Errorf("envFrom: %v", c["envFrom"])
	}
	if !reflect.DeepEqual(c["env"], []any{map[string]any{"name": "PORT", "value": "8080"}}) {
		t.Errorf("env: %v", c["env"])
	}
	if got := dig(dep, "spec", "template", "metadata", "annotations"); !reflect.DeepEqual(got, map[string]any{AnnotationPort: "8080", AnnotationSecrets: "v1", "kubernetes.io/egress-bandwidth": "100M"}) {
		t.Errorf("pod annotations: %v", got)
	}
	if got := dig(dep, "metadata", "annotations"); !reflect.DeepEqual(got, map[string]any{AnnotationPort: "8080"}) {
		t.Errorf("deployment annotations: %v", got)
	}
	// Another version is another pod template; the Secret of a Deployment that does not exist
	// yet has no owner.
	snap.SecretsVersion = "v2"
	if reflect.DeepEqual(dig(dep, "spec", "template"), dig(BuildDeployment(testConfig(), "svc-1", f, snap), "spec", "template")) {
		t.Error("a new secrets version must change the pod template")
	}
	sec := BuildSecret("svc-1", f, "", map[string]string{"TOKEN": "hunter2"})
	if dig(sec, "metadata", "ownerReferences") != nil || digStr(sec, "metadata", "name") != f.ID || digStr(sec, "metadata", "namespace") != "fn-p1" {
		t.Errorf("secret: %v", sec)
	}
	// The values are in data, with the type named: an apply owns the keys it carries, so the next apply without a
	// name takes it away, and no value is ever sent as stringData (which is merged into what is there).
	if sec["type"] != "Opaque" || sec["stringData"] != nil || !reflect.DeepEqual(sec["data"], map[string]any{"TOKEN": "aHVudGVyMg=="}) {
		t.Errorf("secret: %v", sec)
	}
	if raw, _ := json.Marshal(dep); strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "aHVudGVyMg") {
		t.Errorf("a secret value is in the Deployment: %s", raw)
	}
}

// Registry credentials reach the kubelet through the function's pull secret: the Deployment names
// it, and no other, and carries the version marker. Every function pulls on every start, with credentials or
// without: a node's image cache must not stand in for them. The password is in the pull secret's docker config,
// nowhere else.
func TestBuildDeploymentRegistryAuth(t *testing.T) {
	const password = "s3ntinel-password"
	f := Function{ID: "fn-0123456789abcdef", ProjectID: "p1", Host: "api-a1b2c3.sbx.example.com"}
	auth := RegistryAuth{Server: "ghcr.io", Username: "bot", Version: "v1"}
	snap := Snapshot{Image: "ghcr.io/acme/api:1", Port: 8080, CPUMillis: 250, MemoryMi: 256, RegistryAuth: &auth}
	dep := BuildDeployment(testConfig(), "svc-1", f, snap)
	pod := dig(dep, "spec", "template", "spec").(map[string]any)
	if !reflect.DeepEqual(pod["imagePullSecrets"], []any{map[string]any{"name": f.ID + "-registry"}}) {
		t.Errorf("imagePullSecrets: %v", pod["imagePullSecrets"])
	}
	if c := pod["containers"].([]any)[0].(map[string]any); c["imagePullPolicy"] != "Always" || c["envFrom"] != nil {
		t.Errorf("container: %v", c)
	}
	if got := dig(dep, "spec", "template", "metadata", "annotations"); !reflect.DeepEqual(got, map[string]any{AnnotationPort: "8080", AnnotationRegistryAuth: "v1", "kubernetes.io/egress-bandwidth": "100M"}) {
		t.Errorf("pod annotations: %v", got)
	}
	next := snap
	next.RegistryAuth = &RegistryAuth{Server: "ghcr.io", Username: "bot", Version: "v2"}
	if reflect.DeepEqual(dig(dep, "spec", "template"), dig(BuildDeployment(testConfig(), "svc-1", f, next), "spec", "template")) {
		t.Error("new registry credentials must change the pod template")
	}

	// Without credentials there is no pull secret, the platform's template registry included: a function is lent
	// no registry account. The image is still pulled on every start: it may be another function's private one, in
	// a node's cache.
	cfg := testConfig()
	cfg.TemplateRegistry = "registry.example.org/sandbox-templates"
	for _, image := range []string{"nginx:1", "registry.example.org/sandbox-templates/t/p2/tpl:1", "registry.example.org/library/sandbox-base:1"} {
		plain := dig(BuildDeployment(cfg, "svc-1", f, Snapshot{Image: image, Port: 8080, CPUMillis: 250, MemoryMi: 256}), "spec", "template", "spec").(map[string]any)
		if plain["imagePullSecrets"] != nil || plain["containers"].([]any)[0].(map[string]any)["imagePullPolicy"] != "Always" {
			t.Errorf("%s without credentials: %v", image, plain)
		}
	}

	sec := BuildPullSecret("svc-1", f, "uid-1", auth, password)
	owner := []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": f.ID, "uid": "uid-1"}}
	if sec["type"] != "kubernetes.io/dockerconfigjson" || digStr(sec, "metadata", "name") != f.ID+"-registry" || digStr(sec, "metadata", "namespace") != "fn-p1" ||
		!reflect.DeepEqual(dig(sec, "metadata", "ownerReferences"), owner) || digStr(sec, "metadata", "labels", LabelID) != f.ID || len(sec["data"].(map[string]any)) != 1 || sec["stringData"] != nil {
		t.Fatalf("pull secret: %v", sec)
	}
	docker, _ := base64.StdEncoding.DecodeString(digStr(sec, "data", ".dockerconfigjson"))
	if string(docker) != `{"auths":{"ghcr.io":{"password":"`+password+`","username":"bot"}}}` {
		t.Errorf("docker config: %s", docker)
	}
	delete(sec, "data")
	rest, _ := json.Marshal([]any{dep, sec})
	if strings.Contains(string(rest), password) {
		t.Errorf("the password is outside the pull secret's data: %s", rest)
	}
}

// The namespace's quota carries the project's three numbers, on what the pods request (they request what they are
// limited to), and leaves each function one pod more than its most instances for the one a rollout surges.
func TestBuildQuota(t *testing.T) {
	cfg := sandbox.Config{Functions: sandbox.FunctionsConfig{MaxInstances: 1}}
	q := BuildQuota(cfg, "svc-1", "p1", sandbox.FunctionQuota{Functions: 3, VCPUs: 2, RAMMb: 1024})
	want := map[string]any{"count/deployments.apps": "3", "requests.cpu": "2", "requests.memory": "1024Mi", "pods": "6"}
	if got := dig(q, "spec", "hard"); !reflect.DeepEqual(got, want) {
		t.Errorf("hard: %v", got)
	}
	if digStr(q, "metadata", "name") != "proxcloud-function" || digStr(q, "metadata", "namespace") != "fn-p1" || digStr(q, "metadata", "labels", sandbox.LabelProject) != "p1" ||
		digStr(q, "metadata", "labels", sandbox.LabelService) != "svc-1" || q["kind"] != "ResourceQuota" {
		t.Errorf("metadata: %v", q["metadata"])
	}
	// Every function at its most instances, and one surge pod each for a rollout.
	cfg.Functions.MaxInstances = 4
	want["pods"] = "15"
	if got := dig(BuildQuota(cfg, "svc-1", "p1", sandbox.FunctionQuota{Functions: 3, VCPUs: 2, RAMMb: 1024}), "spec", "hard"); !reflect.DeepEqual(got, want) {
		t.Errorf("hard with four instances a function: %v", got)
	}
	// The provider's default for a project that has no limits of its own is never unlimited.
	def := sandbox.Config{}.WithDefaults().Functions
	want = map[string]any{"count/deployments.apps": "20", "requests.cpu": "8", "requests.memory": "16384Mi", "pods": "120"}
	if got := dig(BuildQuota(sandbox.Config{}, "svc-1", "p1", def.ProjectQuota), "spec", "hard"); !reflect.DeepEqual(got, want) {
		t.Errorf("hard by default: %v", got)
	}
}
