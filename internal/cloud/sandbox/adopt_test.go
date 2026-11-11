package sandbox

import "testing"

func prepullPod(node, phase string, images map[string]string, pulled map[string]string) map[string]any {
	var inits, statuses []any
	for name, img := range images {
		inits = append(inits, map[string]any{"name": name, "image": img})
		if id, ok := pulled[name]; ok {
			statuses = append(statuses, map[string]any{"name": name, "imageID": id})
		}
	}
	return map[string]any{
		"metadata": map[string]any{"ownerReferences": []any{map[string]any{"kind": "DaemonSet"}}},
		"spec":     map[string]any{"nodeName": node, "initContainers": inits},
		"status":   map[string]any{"phase": phase, "initContainerStatuses": statuses},
	}
}

func TestAdoptions(t *testing.T) {
	const (
		old    = "reg.io/library/sandbox-base:a@sha256:111"
		neu    = "reg.io/library/sandbox-base:b@sha256:222"
		ci     = "reg.io/library/sandbox-code-interpreter:a@sha256:333"
		ciGPU  = "reg.io/library/sandbox-code-interpreter-gpu:b@sha256:444"
		pulled = "reg.io/library/sandbox-base@sha256:222"
	)
	tpls := map[string]Template{
		"base":                 {Image: old},
		"code-interpreter-v1":  {Image: ci},
		"code-interpreter-gpu": {Image: "reg.io/library/sandbox-code-interpreter-gpu:a@sha256:999"},
		"tagonly":              {Image: "reg.io/library/sandbox-base:latest"},
		"a.b":                  {Image: old},
	}
	done := func(node string) map[string]any {
		return prepullPod(node, "Running", map[string]string{"p0": neu, "p1": ci},
			map[string]string{"p0": pulled, "p1": "reg.io/library/sandbox-code-interpreter@sha256:333"})
	}

	// Every pod pulled the new base: base adopts; code-interpreter-v1 unchanged; gpu image absent.
	got := Adoptions(tpls, []map[string]any{done("n1"), done("n2")}, nil)
	if len(got) != 1 || got[0].Template != "base" || got[0].From != old || got[0].To != neu || !got[0].Ready || got[0].Pulled != 2 {
		t.Fatalf("all pulled: %+v", got)
	}

	// One pod still Pending: not ready, 1/2 pulled.
	pending := prepullPod("n2", "Pending", map[string]string{"p0": neu}, nil)
	got = Adoptions(tpls, []map[string]any{done("n1"), pending}, nil)
	if len(got) != 1 || got[0].Ready || got[0].Pulled != 1 || got[0].Total != 2 {
		t.Fatalf("pending: %+v", got)
	}
	// ...unless its node is not Ready.
	got = Adoptions(tpls, []map[string]any{done("n1"), pending}, map[string]bool{"n2": true})
	if len(got) != 1 || !got[0].Ready {
		t.Fatalf("not-ready node ignored: %+v", got)
	}

	// Rollout in progress: one pod still on the old digest blocks the switch.
	oldPod := prepullPod("n2", "Running", map[string]string{"p0": old}, map[string]string{"p0": "reg.io/library/sandbox-base@sha256:111"})
	got = Adoptions(tpls, []map[string]any{done("n1"), oldPod}, nil)
	if len(got) != 1 || got[0].Ready {
		t.Fatalf("mixed digests: %+v", got)
	}

	// Running but imageID differs (not this digest): not pulled.
	wrong := prepullPod("n1", "Running", map[string]string{"p0": neu}, map[string]string{"p0": "reg.io/library/sandbox-base@sha256:111"})
	got = Adoptions(tpls, []map[string]any{wrong}, nil)
	if len(got) != 1 || got[0].Ready {
		t.Fatalf("wrong imageID: %+v", got)
	}

	// The gpu repo does not match the cpu one (prefix).
	gpuPod := prepullPod("g1", "Running", map[string]string{"p0": ciGPU}, map[string]string{"p0": "reg.io/library/sandbox-code-interpreter-gpu@sha256:444"})
	got = Adoptions(tpls, []map[string]any{gpuPod}, nil)
	if len(got) != 1 || got[0].Template != "code-interpreter-gpu" || !got[0].Ready {
		t.Fatalf("gpu: %+v", got)
	}
}

func TestSplitRef(t *testing.T) {
	for ref, want := range map[string][2]string{
		"reg.io:5000/a/b:t@sha256:1": {"reg.io:5000/a/b", "sha256:1"},
		"reg.io:5000/a/b@sha256:1":   {"reg.io:5000/a/b", "sha256:1"},
		"reg.io/a/b:t":               {"", ""},
	} {
		if r, d := splitRef(ref); r != want[0] || d != want[1] {
			t.Errorf("%s: %s %s", ref, r, d)
		}
	}
}
