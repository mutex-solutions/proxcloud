package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// dsFake adds DaemonSets (with resourceVersion optimistic concurrency) to fakeAPI.
type dsFake struct {
	*fakeAPI
	ds      map[string]map[string]any
	patches []map[string]any
	logs    string
}

func (f *dsFake) GetDaemonSet(_ context.Context, ns, name string) (map[string]any, error) {
	d := f.ds[ns+"/"+name]
	if d == nil {
		return nil, nil
	}
	b, _ := json.Marshal(d)
	var cp map[string]any
	_ = json.Unmarshal(b, &cp)
	return cp, nil
}

func (f *dsFake) PatchDaemonSet(_ context.Context, ns, name string, patch map[string]any) (map[string]any, error) {
	d := f.ds[ns+"/"+name]
	if d == nil {
		return nil, errors.New("404")
	}
	if digStr(patch, "metadata", "resourceVersion") != digStr(d, "metadata", "resourceVersion") {
		return nil, errors.New("409 conflict")
	}
	f.patches = append(f.patches, patch)
	d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["initContainers"] = dig(patch, "spec", "template", "spec", "initContainers")
	md := d["metadata"].(map[string]any)
	md["resourceVersion"] = md["resourceVersion"].(string) + "1"
	return d, nil
}

func (f *dsFake) PodLogs(_ context.Context, _, _, _ string, _ int) (string, error) {
	return f.logs, nil
}

const mirrorReg = "192.0.2.226"

var (
	dOld = "sha256:" + strings.Repeat("1", 64)
	dNew = "sha256:" + strings.Repeat("2", 64)
)

func prepullDS(name string, images ...string) map[string]any {
	var cs []any
	for i, img := range images {
		cs = append(cs, map[string]any{"name": "p" + string(rune('0'+i)), "image": img, "command": []any{"sh", "-c", "true"},
			"resources": map[string]any{"limits": map[string]any{"cpu": "100m"}}})
	}
	return map[string]any{"metadata": map[string]any{"name": name, "namespace": PrepullNamespace, "resourceVersion": "7"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"initContainers": cs,
			"containers": []any{map[string]any{"name": "pause", "image": "pause"}}}}}}
}

func mirrorFixture() (*Service, *dsFake) {
	f := &dsFake{fakeAPI: newFake(), ds: map[string]map[string]any{}}
	cfg := testConfig()
	cfg.TemplateRegistry = mirrorReg + "/t"
	cfg.RegistryMirror = RegistryCredential{User: RegistryMirrorUser, Password: "mirror-pw"}
	f.secrets[PrepullNamespace+"/sandbox-import-worker"] = map[string][]byte{"image": []byte(mirrorReg + "/library/proxcloud-sandbox-import@" + importDigest)}
	f.ds[PrepullNamespace+"/"+PrepullDaemonSet] = prepullDS(PrepullDaemonSet,
		mirrorReg+"/library/python:3.12-slim@"+dOld, mirrorReg+"/library/sandbox-base:20261101-aaaaaaa@"+dOld)
	return NewWithAPI(f, cfg, "svc-sbx"), f
}

func TestMirrorWorkerPodAndPodOwnedSecrets(t *testing.T) {
	s, f := mirrorFixture()
	req := MirrorRequest{JobShort: "0123456789abcdef", Epoch: 2, Source: "ghcr.io/mutex-solutions/sandbox-base@" + dNew,
		Destination: mirrorReg + "/library/sandbox-base:20261112-9d00563", GitHubToken: "ghp_secret_pat"}
	uid, err := s.StartMirror(context.Background(), req)
	if err != nil || uid == "" {
		t.Fatalf("start: %q %v", uid, err)
	}
	pod := f.core["pods/"+BuildNamespace+"/tu-mirror-0123456789abcdef-e2"]
	if pod == nil {
		t.Fatal("no worker pod")
	}
	pj, _ := json.Marshal(pod)
	if strings.Contains(string(pj), "ghp_secret_pat") || strings.Contains(string(pj), "mirror-pw") {
		t.Fatal("a credential is in the pod spec")
	}
	c := dig(pod, "spec", "containers").([]any)[0].(map[string]any)
	args := c["args"].([]any)
	if c["command"].([]any)[0] != "/sandbox-import" || len(args) != 4 || args[0] != "mirror" || args[1] != req.Source || args[2] != req.Destination {
		t.Fatalf("argv %v", args)
	}
	if dig(pod, "spec", "automountServiceAccountToken") != false || digStr(pod, "spec", "imagePullSecrets") != "" &&
		dig(pod, "spec", "imagePullSecrets").([]any)[0].(map[string]any)["name"] != MirrorPullSecret {
		t.Fatal("pod hardening")
	}
	for _, which := range []string{"source", "push"} {
		sec := f.core["secrets/"+BuildNamespace+"/tu-mirror-0123456789abcdef-e2-"+which]
		owners, _ := dig(sec, "metadata", "ownerReferences").([]any)
		if sec == nil || len(owners) != 1 || owners[0].(map[string]any)["uid"] != uid || owners[0].(map[string]any)["kind"] != "Pod" {
			t.Fatalf("%s secret not owned by the pod: %v", which, sec)
		}
		raw, _ := base64.StdEncoding.DecodeString(digStr(sec, "data", ".dockerconfigjson"))
		host := mirrorReg
		if which == "source" {
			host = "ghcr.io"
		}
		var dc map[string]map[string]map[string]string
		_ = json.Unmarshal(raw, &dc)
		if len(dc["auths"]) != 1 || dc["auths"][host]["password"] == "" {
			t.Fatalf("%s secret holds %v", which, dc)
		}
	}
	// Prerequisites fail closed.
	for name, mut := range map[string]func(*Service, *dsFake){
		"no mirror account": func(s *Service, _ *dsFake) { s.cfg.RegistryMirror = RegistryCredential{} },
		"no registry":       func(s *Service, _ *dsFake) { s.cfg.TemplateRegistry = "" },
		"tag worker": func(_ *Service, f *dsFake) {
			f.secrets[PrepullNamespace+"/sandbox-import-worker"] = map[string][]byte{"image": []byte(mirrorReg + "/library/proxcloud-sandbox-import:latest")}
		},
		"foreign worker": func(_ *Service, f *dsFake) {
			f.secrets[PrepullNamespace+"/sandbox-import-worker"] = map[string][]byte{"image": []byte("evil.test/library/proxcloud-sandbox-import@" + importDigest)}
		},
	} {
		s, f := mirrorFixture()
		mut(s, f)
		if _, err := s.StartMirror(context.Background(), req); !errors.Is(err, ErrMirrorUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s, _ = mirrorFixture()
	for _, bad := range []MirrorRequest{
		{JobShort: req.JobShort, Epoch: 1, Source: req.Source, Destination: mirrorReg + "/library/proxcloud-api:20261112-9d00563", GitHubToken: "x"},
		{JobShort: req.JobShort, Epoch: 1, Source: "ghcr.io/other/sandbox-base@" + dNew, Destination: req.Destination, GitHubToken: "x"},
		{JobShort: "NOTHEX", Epoch: 1, Source: req.Source, Destination: req.Destination, GitHubToken: "x"},
		{JobShort: req.JobShort, Epoch: 1, Source: req.Source, Destination: req.Destination, GitHubToken: ""},
	} {
		if _, err := s.StartMirror(context.Background(), bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestMirrorResultValidation(t *testing.T) {
	s, f := mirrorFixture()
	p := &MirrorPod{Name: "tu-mirror-0123456789abcdef-e1", Source: "ghcr.io/mutex-solutions/sandbox-base@" + dNew, Dest: mirrorReg + "/library/sandbox-base:20261112-9d00563"}
	good := map[string]any{"source": p.Source, "destination": p.Dest, "sourceDigest": dNew, "destinationDigest": dNew, "platform": "linux/amd64", "sizeBytes": 10, "configDigest": dOld}
	b, _ := json.Marshal(good)
	f.logs = "2026-11-12T00:00:00Z " + string(b)
	if r, err := s.MirrorResult(context.Background(), p); err != nil || r.DestinationDigest != dNew {
		t.Fatalf("good result: %v %v", r, err)
	}
	good["destinationDigest"] = dOld
	b, _ = json.Marshal(good)
	f.logs = string(b)
	if _, err := s.MirrorResult(context.Background(), p); err == nil {
		t.Fatal("a result with another destination digest was accepted")
	}
}

func TestSetPrepullImageReplacesOnlyThatRepository(t *testing.T) {
	s, f := mirrorFixture()
	ref := mirrorReg + "/library/sandbox-base:20261112-9d00563@" + dNew
	prev, changed, err := s.SetPrepullImage(context.Background(), ref)
	if err != nil || !changed || len(prev) != 1 || prev[0].Image != mirrorReg+"/library/sandbox-base:20261101-aaaaaaa@"+dOld || prev[0].Container != "p1" {
		t.Fatalf("replace: %v %v %v", prev, changed, err)
	}
	cs := initContainers(f.ds[PrepullNamespace+"/"+PrepullDaemonSet])
	if len(cs) != 2 || cs[1]["image"] != ref || cs[0]["image"] != mirrorReg+"/library/python:3.12-slim@"+dOld || cs[1]["name"] != "p1" {
		t.Fatalf("init containers %v", cs)
	}
	if dig(cs[1], "resources", "limits", "cpu") != "100m" {
		t.Fatal("the init container lost its other fields")
	}
	if p := f.patches[0]; digStr(p, "metadata", "resourceVersion") != "7" || len(p) != 2 || len(dig(p, "spec").(map[string]any)) != 1 {
		t.Fatalf("patch shape %v", p)
	}
	// Idempotent: same ref again writes nothing.
	if _, changed, err = s.SetPrepullImage(context.Background(), ref); err != nil || changed || len(f.patches) != 1 {
		t.Fatalf("re-set: %v %v", changed, err)
	}
	// A repository no DaemonSet pulls yet gets one new init container named after the installer's p<N>.
	nodeRef := mirrorReg + "/library/sandbox-node:20261112-9d00563@" + dNew
	if prev, changed, err = s.SetPrepullImage(context.Background(), nodeRef); err != nil || !changed || len(prev) != 0 {
		t.Fatalf("add: %v %v %v", prev, changed, err)
	}
	cs = initContainers(f.ds[PrepullNamespace+"/"+PrepullDaemonSet])
	if len(cs) != 3 || cs[2]["name"] != "p2" || cs[2]["image"] != nodeRef || dig(cs[2], "command") == nil {
		t.Fatalf("added %v", cs)
	}
	// The GPU DaemonSet is patched too when it pulls the repository.
	f.ds[PrepullNamespace+"/"+PrepullGPUDaemonSet] = prepullDS(PrepullGPUDaemonSet, mirrorReg+"/library/sandbox-base:20261101-aaaaaaa@"+dOld)
	if prev, changed, err = s.SetPrepullImage(context.Background(), mirrorReg+"/library/sandbox-base:20261113-bbbbbbb@"+dOld); err != nil || !changed || len(prev) != 2 || prev[1].DaemonSet != PrepullGPUDaemonSet {
		t.Fatalf("gpu: %v %v %v", prev, changed, err)
	}
	// Refused refs never reach the API.
	before := len(f.patches)
	for _, bad := range []string{
		"ghcr.io/mutex-solutions/sandbox-base@" + dNew, mirrorReg + "/library/proxcloud-sandbox-proxy@" + dNew,
		mirrorReg + "/library/sandbox-base:20261112-9d00563", mirrorReg + "/t/p/x@" + dNew, "198.51.100.1/library/sandbox-base@" + dNew,
	} {
		if _, _, err := s.SetPrepullImage(context.Background(), bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if len(f.patches) != before {
		t.Fatal("a refused ref was patched")
	}
}

func TestPrepullProgressCountsReadyNodes(t *testing.T) {
	s, f := mirrorFixture()
	ref := mirrorReg + "/library/sandbox-base:20261112-9d00563@" + dNew
	pod := func(node, image, imageID, phase string) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": "pp-" + node, "ownerReferences": []any{map[string]any{"kind": "DaemonSet"}}},
			"spec":   map[string]any{"nodeName": node, "initContainers": []any{map[string]any{"name": "p1", "image": image}}},
			"status": map[string]any{"phase": phase, "initContainerStatuses": []any{map[string]any{"name": "p1", "imageID": imageID}}}}
	}
	f.core["pods/"+PrepullNamespace+"/a"] = pod("a", ref, "x@"+dNew, "Running")
	f.core["pods/"+PrepullNamespace+"/b"] = pod("b", mirrorReg+"/library/sandbox-base:x@"+dOld, "x@"+dOld, "Running")
	f.core["pods/"+PrepullNamespace+"/c"] = pod("c", ref, "", "Pending")
	f.core["pods/"+PrepullNamespace+"/d"] = pod("d", ref, "", "Pending") // NotReady node: not counted
	ready := func(n string, ok string) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": n}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": ok}}}}
	}
	f.nodes = []map[string]any{ready("a", "True"), ready("b", "True"), ready("c", "True"), ready("d", "False")}
	pulled, total, err := s.PrepullProgress(context.Background(), ref)
	if err != nil || pulled != 1 || total != 3 {
		t.Fatalf("progress %d/%d %v", pulled, total, err)
	}
}
