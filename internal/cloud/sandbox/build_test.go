package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRenderDockerfile(t *testing.T) {
	steps := []BuildStep{
		{Type: "RUN", Args: []string{"apt-get update && apt-get install -y curl"}},
		{Type: "ENV", Args: []string{"A", "1", "B", `has "quotes" and spaces`}},
		{Type: "COPY", Args: []string{"app/", "/home/user/app", "user", "0755"}, FilesHash: "abc12345"},
		{Type: "COPY", Args: []string{"app/main.py", "/srv/"}, FilesHash: "abc12345"},
		{Type: "WORKDIR", Args: []string{"/home/user/app"}},
		{Type: "RUN", Args: []string{"pip install -r requirements.txt", "user"}},
		{Type: "USER", Args: []string{"user"}},
	}
	res, err := RenderDockerfile("python:3.12-slim", steps)
	if err != nil {
		t.Fatal(err)
	}
	want := `FROM python:3.12-slim
RUN ["/bin/sh", "-c", "apt-get update && apt-get install -y curl"]
ENV A="1" B="has \"quotes\" and spaces"
COPY --chown=user:user --chmod=0755 ["files/abc12345/app", "/home/user/app"]
COPY ["files/abc12345/app/main.py", "/srv/"]
WORKDIR /home/user/app
USER user
RUN ["/bin/sh", "-c", "pip install -r requirements.txt"]
USER root
USER user
USER root
`
	if res.Dockerfile != want {
		t.Errorf("Dockerfile:\n%s\nwant:\n%s", res.Dockerfile, want)
	}
	if len(res.Hashes) != 1 || res.User != "user" || res.Workdir != "/home/user/app" {
		t.Errorf("result = %+v", res)
	}

	// Nothing a caller sends may start a new instruction or escape the context.
	for name, st := range map[string]BuildStep{
		"newline in workdir": {Type: "WORKDIR", Args: []string{"/x\nRUN evil"}},
		"newline in env":     {Type: "ENV", Args: []string{"A", "1\nRUN evil"}},
		"bad env key":        {Type: "ENV", Args: []string{"A B", "1"}},
		"bad user":           {Type: "USER", Args: []string{"root\nRUN evil"}},
		"copy escape":        {Type: "COPY", Args: []string{"../etc/passwd", "/x"}, FilesHash: "abc12345"},
		"copy absolute":      {Type: "COPY", Args: []string{"/etc/passwd", "/x"}, FilesHash: "abc12345"},
		"copy no hash":       {Type: "COPY", Args: []string{"a", "/x"}},
		"bad mode":           {Type: "COPY", Args: []string{"a", "/x", "", "9999"}, FilesHash: "abc12345"},
		"unknown":            {Type: "ADD", Args: []string{"x"}},
		"empty run":          {Type: "RUN", Args: []string{" "}},
	} {
		if _, err := RenderDockerfile("python:3.12", []BuildStep{st}); !errors.Is(err, ErrBadBuild) {
			t.Errorf("%s: err = %v, want ErrBadBuild", name, err)
		}
	}
	if _, err := RenderDockerfile("python:3.12\nRUN evil", nil); !errors.Is(err, ErrBadBuild) {
		t.Error("base image with a newline must be rejected")
	}
}

func TestBuildObjects(t *testing.T) {
	res, _ := RenderDockerfile("python:3.12", []BuildStep{{Type: "COPY", Args: []string{"a", "/a"}, FilesHash: "abc12345"}})
	cm, pod := BuildObjects(Config{}, BuildRequest{
		ProjectID: "p1", TemplateID: "t1", BuildID: "b1", Image: testRegistry + "/p1/t1:b1",
		Result: res, CPU: 2, MemoryGiB: 4, FetchURL: func(h string) string { return "https://api/fetch/" + h },
	})
	if !strings.Contains(digStr(cm, "data", "fetch.list"), "abc12345 https://api/fetch/abc12345") {
		t.Errorf("fetch list: %v", cm["data"])
	}
	b, _ := json.Marshal(pod)
	js := string(b)
	for _, want := range []string{`"runtimeClassName":"gvisor"`, `"automountServiceAccountToken":false`, `"--no-push"`, `"activeDeadlineSeconds":1800`} {
		if !strings.Contains(js, want) {
			t.Errorf("pod lacks %s", want)
		}
	}
	// The push secret is mounted ONLY in the push container: user code (kaniko) never sees it.
	inits, _ := dig(pod, "spec", "initContainers").([]any)
	for _, c := range inits {
		cb, _ := json.Marshal(c)
		if strings.Contains(string(cb), "push-secret") {
			t.Errorf("init container %s mounts the push secret", digStr(c.(map[string]any), "name"))
		}
	}
}

func TestBuildState(t *testing.T) {
	pod := func(phase string, status map[string]any) map[string]any {
		st := map[string]any{"phase": phase, "startTime": "2026-11-06T06:00:00Z"}
		for k, v := range status {
			st[k] = v
		}
		return map[string]any{
			"metadata": map[string]any{"labels": map[string]any{LabelTemplateID: "t1"}},
			"spec":     map[string]any{"initContainers": []any{map[string]any{"name": "kaniko", "resources": map[string]any{"limits": map[string]any{"cpu": "2", "memory": "4Gi"}}}}},
			"status":   st,
		}
	}
	ok := pod("Succeeded", map[string]any{"containerStatuses": []any{map[string]any{"name": "push", "state": map[string]any{"terminated": map[string]any{
		"exitCode": 0, "finishedAt": "2026-11-06T06:02:00Z", "message": testRegistry + "/p1/t1:b1@sha256:abcd\n"}}}}})
	if s := buildStateOf(ok); s.Status != BuildReady || s.Digest != "sha256:abcd" {
		t.Errorf("ready: %+v", s)
	}
	if d := buildData(ok); d["status"] != StatusDeleted || d["endedAt"] != "2026-11-06T06:02:00Z" || d["cpu"] != 2 || d["kind"] != "build" {
		t.Errorf("finished build payload: %v", d)
	}
	failed := pod("Failed", map[string]any{"initContainerStatuses": []any{
		map[string]any{"name": "fetch", "state": map[string]any{"terminated": map[string]any{"exitCode": 0}}},
		map[string]any{"name": "kaniko", "state": map[string]any{"terminated": map[string]any{"exitCode": 1, "finishedAt": "2026-11-06T06:01:00Z"}}},
	}})
	if s := buildStateOf(failed); s.Status != BuildError || s.Step != "kaniko" || !strings.Contains(s.Reason, "build step") {
		t.Errorf("failed: %+v", s)
	}
	if s := buildStateOf(pod("Running", nil)); s.Status != BuildBuilding {
		t.Errorf("running: %+v", s)
	}
	if d := buildData(pod("Running", nil)); d["status"] != StatusRunning || d["runningSince"] != "2026-11-06T06:00:00Z" {
		t.Errorf("running payload: %v", d)
	}
}

// A build pod finished long ago is deleted by the listing, but still reported once.
func TestListBuildsCleansUp(t *testing.T) {
	f := newFake()
	s := NewWithAPI(f, Config{Templates: map[string]Template{"py": {Image: "x"}}}, "svc")
	s.now = func() time.Time { return time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC) }
	pod := map[string]any{
		"metadata": map[string]any{"name": "build-b1", "namespace": BuildNamespace, "labels": map[string]any{LabelBuild: "b1", LabelProject: "p1", LabelTemplateID: "t1"}},
		"status": map[string]any{"phase": "Succeeded", "startTime": "2026-11-06T07:00:00Z", "containerStatuses": []any{map[string]any{"name": "push",
			"state": map[string]any{"terminated": map[string]any{"exitCode": 0, "finishedAt": "2026-11-06T07:05:00Z", "message": "r@sha256:1"}}}}},
	}
	_ = f.ApplyCore(context.Background(), "pods", pod)
	got, err := s.ListBuilds(context.Background(), "p1")
	if err != nil || got["build-b1"]["status"] != StatusDeleted {
		t.Fatalf("finished build must still be reported: %v %v", got, err)
	}
	if p, _ := f.GetCore(context.Background(), "pods", BuildNamespace, "build-b1"); p != nil {
		t.Error("a build pod finished an hour ago must be deleted")
	}
}

func TestCloneSaved(t *testing.T) {
	cfg := Config{Envd: true, Templates: map[string]Template{"py": {Image: "python:3.12", CPU: 1, MemoryGiB: 2}}}
	pub := false
	cr := BuildCR(cfg, "svc", Spec{ID: "sbx-00000001", ProjectID: "p1", Template: "py", CPU: 1, MemoryGiB: 2,
		Env: map[string]string{"A": "1"}, AllowPublicTraffic: &pub, envdToken: "tok", trafficToken: "ttok"}, time.Unix(0, 0))
	clone, id := CloneSaved(cr)
	if id == "sbx-00000001" || digStr(clone, "metadata", "name") != id || digStr(clone, "spec", "podTemplate", "metadata", "labels", LabelSandboxID) != id {
		t.Fatalf("clone id not applied: %s", id)
	}
	b, _ := json.Marshal(clone)
	if strings.Contains(string(b), `"tok"`) || strings.Contains(string(b), `"ttok"`) {
		t.Error("a fork must not share the source's tokens")
	}
	if !strings.Contains(string(b), `"name":"A","value":"1"`) {
		t.Error("a fork keeps the source's env")
	}
	if digStr(cr, "metadata", "name") != "sbx-00000001" {
		t.Error("CloneSaved must not modify its input")
	}
}

func TestAutoPauseLifecycle(t *testing.T) {
	f := newFake()
	s := NewWithAPI(f, Config{Templates: map[string]Template{"py": {Image: "python:3.12", CPU: 1, MemoryGiB: 2}}}, "svc")
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	sp := Spec{ProjectID: "p1", Template: "py", TimeoutSeconds: 600, AutoPause: true}
	if _, err := s.CreateSandbox(context.Background(), &sp); err != nil {
		t.Fatal(err)
	}
	cr := f.crs[NamespaceFor("p1")+"/"+sp.ID]
	if digStr(cr, "metadata", "annotations", AnnotationEndAt) != "2026-11-06T10:10:00Z" || digStr(cr, "spec", "shutdownTime") != "2026-11-06T10:25:00Z" {
		t.Fatalf("auto-pause end/shutdown: %v / %v", digStr(cr, "metadata", "annotations", AnnotationEndAt), digStr(cr, "spec", "shutdownTime"))
	}
	if d := sandboxData(cr); d["shutdownTime"] != "2026-11-06T10:10:00Z" {
		t.Errorf("the customer-facing end is end-at: %v", d["shutdownTime"])
	}
	if due, _ := s.DueForAutoPause(context.Background()); len(due) != 0 {
		t.Errorf("not due yet: %v", due)
	}
	now = now.Add(11 * time.Minute)
	if due, _ := s.DueForAutoPause(context.Background()); len(due) != 1 || due[0].ID != sp.ID || due[0].ProjectID != "p1" {
		t.Errorf("due after end-at: %v", due)
	}
}

func TestResumeFromPublicImageGetsPullSecret(t *testing.T) {
	f := newFake()
	cfg := Config{Templates: map[string]Template{"py": {Image: "python:3.12", CPU: 1, MemoryGiB: 2}}}
	img := testRegistry + "/p1/t1@sha256:1"
	// No template registry configured: the same image is nobody's private image.
	cr := BuildCR(cfg, "svc", Spec{ID: "sbx-00000001", ProjectID: "p1", Template: "py", CPU: 1, MemoryGiB: 2}, time.Unix(0, 0))
	if err := NewWithAPI(f, cfg, "svc").ResumeSandbox(context.Background(), "p1", cr, img, time.Unix(3600, 0), nil); err != nil {
		t.Fatal(err)
	}
	if got := f.crs[NamespaceFor("p1")+"/sbx-00000001"]; dig(got, "spec", "podTemplate", "spec", "imagePullSecrets") != nil {
		t.Fatal("no templateRegistry configured, yet the image got the template pull secret")
	}

	cfg.TemplateRegistry = testRegistry
	s := NewWithAPI(f, cfg, "svc")
	cr = BuildCR(cfg, "svc", Spec{ID: "sbx-00000001", ProjectID: "p1", Template: "py", CPU: 1, MemoryGiB: 2}, time.Unix(0, 0))
	if dig(cr, "spec", "podTemplate", "spec", "imagePullSecrets") != nil {
		t.Fatal("a public image needs no pull secret")
	}
	if err := s.ResumeSandbox(context.Background(), "p1", cr, img, time.Unix(3600, 0), nil); err != nil {
		t.Fatal(err)
	}
	got := f.crs[NamespaceFor("p1")+"/sbx-00000001"]
	if firstContainer(got)["image"] != img || dig(got, "spec", "podTemplate", "spec", "imagePullSecrets") == nil {
		t.Errorf("resumed onto the private snapshot image without a pull secret: %v", dig(got, "spec", "podTemplate", "spec"))
	}
}

// A built-in image mirrored to the registry (library/**) needs the project's pull Secret too: the kubelet checks it
// even for an image the prepull pod already pulled. The portal never writes that Secret itself any more (the
// registry's account sync does, one account per project), so a shared source in sandbox-system is not copied.
func TestRegistryImagesGetTheProjectPullSecret(t *testing.T) {
	cfg := Config{TemplateRegistry: "192.0.2.10/t", Templates: map[string]Template{
		"base": {Image: "192.0.2.10/library/sandbox-base:1@sha256:" + strings.Repeat("b", 64), CPU: 1, MemoryGiB: 1},
		"py":   {Image: "docker.io/library/python:3.12", CPU: 1, MemoryGiB: 1},
		"evil": {Image: "192.0.2.100/library/x:1", CPU: 1, MemoryGiB: 1},
	}}
	for tpl, want := range map[string]bool{"base": true, "py": false, "evil": false} {
		cr := BuildCR(cfg, "svc", Spec{ID: "sbx-00000001", ProjectID: "p1", Template: tpl, CPU: 1, MemoryGiB: 1}, time.Unix(0, 0))
		got := dig(cr, "spec", "podTemplate", "spec", "imagePullSecrets")
		if (got != nil) != want {
			t.Errorf("%s: imagePullSecrets %v, want present=%v", tpl, got, want)
		}
	}
	f := newFake()
	f.secrets["sandbox-system/"+TemplatePullSecret] = map[string][]byte{".dockerconfigjson": []byte(`{"auths":{}}`)}
	if err := NewWithAPI(f, cfg, "svc").EnsureProjectNamespace(context.Background(), "p1", nil); err != nil {
		t.Fatal(err)
	}
	if sec, _ := f.GetCore(context.Background(), "secrets", NamespaceFor("p1"), TemplatePullSecret); sec != nil {
		t.Error("the shared sandbox-system pull secret was copied into the project namespace")
	}
}

// TestBuildBaseCredentials: only the `base` init container ever mounts a registry credential, and
// only when the build needs one; kaniko (user RUN steps) builds offline from base's cache.
func TestBuildBaseCredentials(t *testing.T) {
	res, _ := RenderDockerfile("python:3.12", []BuildStep{{Type: "RUN", Args: []string{"echo hi"}}})
	containers := func(pod map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, k := range []string{"initContainers", "containers"} {
			cs, _ := dig(pod, "spec", k).([]any)
			for _, c := range cs {
				m := c.(map[string]any)
				out[str(m["name"])] = m
			}
		}
		return out
	}
	mounts := func(c map[string]any) string { b, _ := json.Marshal(c["volumeMounts"]); return string(b) }
	for auth, secret := range map[BaseAuth]string{BaseAuthNone: "", BaseAuthRegistry: "registry-build-p1", BaseAuthUser: "build-b1-base"} {
		_, pod := BuildObjects(Config{}, BuildRequest{ProjectID: "p1", TemplateID: "t1", BuildID: "b1", Image: testRegistry + "/p1/t1:b1",
			Result: res, Base: "python:3.12", BaseAuth: auth, FetchURL: func(string) string { return "" }})
		cs := containers(pod)
		order := []string{}
		for _, c := range dig(pod, "spec", "initContainers").([]any) {
			order = append(order, str(c.(map[string]any)["name"]))
		}
		if strings.Join(order, ",") != "fetch,base,kaniko" {
			t.Fatalf("init order = %v", order)
		}
		for _, name := range []string{"fetch", "kaniko"} {
			if m := mounts(cs[name]); strings.Contains(m, "secret") || strings.Contains(m, "/docker") {
				t.Errorf("auth %d: %s mounts a credential: %s", auth, name, m)
			}
		}
		vols, _ := json.Marshal(dig(pod, "spec", "volumes"))
		hasBase := strings.Contains(mounts(cs["base"]), "base-secret")
		if (secret != "") != hasBase || (secret != "" && !strings.Contains(string(vols), `"secretName":"`+secret+`"`)) {
			t.Errorf("auth %d: base secret mount=%v, volumes=%s", auth, hasBase, vols)
		}
		args, _ := json.Marshal(cs["kaniko"]["args"])
		for _, want := range []string{`"--dockerfile=/pinned/Dockerfile"`, `"--cache=true"`, `"--cache-dir=/cache"`, `"--credential-helpers="`} {
			if !strings.Contains(string(args), want) {
				t.Errorf("kaniko args lack %s: %s", want, args)
			}
		}
		if env, _ := json.Marshal(cs["base"]["env"]); !strings.Contains(string(env), `"name":"SRC","value":"python:3.12"`) {
			t.Errorf("base env = %s", env)
		}
	}
}

func TestStartBuildUserSecretOwnedByPod(t *testing.T) {
	f := newFake()
	s := NewWithAPI(f, Config{}, "svc")
	res, _ := RenderDockerfile("ghcr.io/acme/app:1", nil)
	err := s.StartBuild(context.Background(), BuildRequest{ProjectID: "p1", TemplateID: "t1", BuildID: "b1", Image: testRegistry + "/p1/t1:b1",
		Result: res, Base: "ghcr.io/acme/app:1", BaseAuth: BaseAuthUser, BaseDockerConfig: []byte(`{"auths":{}}`), FetchURL: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := f.GetCore(context.Background(), "secrets", BuildNamespace, "build-b1-base")
	if sec == nil {
		t.Fatal("no credentials secret")
	}
	owners, _ := dig(sec, "metadata", "ownerReferences").([]any)
	if len(owners) != 1 || str(owners[0].(map[string]any)["kind"]) != "Pod" || str(owners[0].(map[string]any)["name"]) != "build-b1" {
		t.Errorf("secret must be owned by the build pod: %v", owners)
	}
}

// A build mounts the project's own build account (written by the registry's account sync), never a shared one: the
// push container always, the base container for an image on the sandbox registry. StartBuild makes the project's
// namespace first (the sync derives the accounts from it), leaves its quota alone and writes no Secret itself.
func TestStartBuildUsesTheProjectBuildAccount(t *testing.T) {
	f := newFake()
	f.secrets["sandbox-system/"+TemplatePullSecret] = map[string][]byte{".dockerconfigjson": []byte(`{"auths":{}}`)}
	s := NewWithAPI(f, Config{}, "svc")
	base := testRegistry + "/p1/t0@sha256:" + strings.Repeat("a", 64)
	res, _ := RenderDockerfile(base, nil)
	if err := s.StartBuild(context.Background(), BuildRequest{ProjectID: "p1", TemplateID: "t1", BuildID: "b1", Image: testRegistry + "/p1/t1:b1",
		Result: res, Base: base, BaseAuth: BaseAuthRegistry, FetchURL: func(string) string { return "" }}); err != nil {
		t.Fatal(err)
	}
	if l := f.namespaces[NamespaceFor("p1")]; l[LabelTenant] != "true" || l[LabelService] != "svc" {
		t.Errorf("the project namespace was not made: %v", l)
	}
	if len(f.quotas) != 0 {
		t.Errorf("a build must not touch the project's quota: %v", f.quotas)
	}
	pod, _ := f.GetCore(context.Background(), "pods", BuildNamespace, "build-b1")
	if vols, _ := json.Marshal(dig(pod, "spec", "volumes")); strings.Count(string(vols), `"secretName":"registry-build-p1"`) != 2 ||
		!strings.Contains(string(vols), `"configMap":{"name":"registry-ca","optional":true}`) {
		t.Errorf("push and base must mount the project's build account and the registry CA: %s", vols)
	}
	// crane in base and push trusts the registry's own CA (Zot's certificate is not from a public CA).
	for _, k := range []string{"initContainers", "containers"} {
		for _, c := range dig(pod, "spec", k).([]any) {
			m := c.(map[string]any)
			env, _ := json.Marshal(m["env"])
			if n := str(m["name"]); (n == "base" || n == "push") != strings.Contains(string(env), `"SSL_CERT_DIR"`) {
				t.Errorf("%s: SSL_CERT_DIR %s", n, env)
			}
		}
	}
	for k := range f.core {
		if strings.HasPrefix(k, "secrets/") {
			t.Errorf("StartBuild wrote a Secret %s", k)
		}
	}
	snap := SnapshotObjects(Config{}, SnapshotRequest{ProjectID: "p2", TemplateID: "t", BuildID: "b2", Base: "x", Image: "y", LayerURL: "z"})
	if v, _ := json.Marshal(dig(snap, "spec", "volumes")); !strings.Contains(string(v), `"secretName":"registry-build-p2"`) {
		t.Errorf("a snapshot pushes with its project's build account: %s", v)
	}
}

func TestTemplateOfSavedKeepsStartAsRoot(t *testing.T) {
	cfg := Config{Envd: true, Templates: map[string]Template{"ci": {Image: "ci", CPU: 1, MemoryGiB: 1, StartCmd: "serve", StartAsRoot: true}}}
	cr := BuildCR(cfg, "svc", Spec{ID: "sbx-00000001", ProjectID: "p1", Template: "ci", CPU: 1, MemoryGiB: 1}, time.Unix(0, 0))
	if tm := TemplateOfSaved(cr); !tm.StartAsRoot || tm.StartCmd != "serve" {
		t.Errorf("a snapshot of a startAsRoot sandbox must keep it: %+v", tm)
	}
	if got := (Template{Command: []string{"sleep", "infinity"}}).Start(); got != "'sleep' 'infinity'" && got != "sleep infinity" {
		t.Errorf("Start() from Command = %q", got)
	}
}

// testRegistry stands in for a provider's config.sandbox.templateRegistry.
const testRegistry = "registry.example.org/sandbox-templates"
