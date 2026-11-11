package sandbox

import (
	"context"
	"testing"
)

type replacedCleanupAPI struct {
	*fakeAPI
	replacementUID string
}

func (f *replacedCleanupAPI) DeleteCoreUID(ctx context.Context, p, ns, name, uid string) error {
	obj := f.core[p+"/"+ns+"/"+name]
	obj["metadata"].(map[string]any)["uid"] = f.replacementUID
	obj["status"] = map[string]any{"phase": "Running"}
	return f.fakeAPI.DeleteCoreUID(ctx, p, ns, name, uid)
}
func TestTerminalBuildCleanupCannotDeleteReplacementUID(t *testing.T) {
	f := &replacedCleanupAPI{fakeAPI: newFake(), replacementUID: "new-active"}
	pod := diagnosticPod("Failed", terminated("kaniko", 1, "Error", ""))
	_ = f.ApplyCore(context.Background(), "pods", pod)
	s := NewWithAPI(f, Config{}, "svc")
	if err := s.CleanupBuild(context.Background(), "p1", "b1"); err == nil {
		t.Fatal("replacement cleanup unexpectedly succeeded")
	}
	if got := f.core["pods/"+BuildNamespace+"/build-b1"]; got == nil || digStr(got, "metadata", "uid") != "new-active" {
		t.Fatal("replacement deleted", got)
	}
}
func TestTerminalImportCleanupCannotDeleteReplacementUID(t *testing.T) {
	cfg, base, req := importFixture()
	f := &replacedCleanupAPI{fakeAPI: base, replacementUID: "new-import-active"}
	s := NewWithAPI(f, cfg, "svc")
	if err := s.StartImport(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	base.core["pods/"+BuildNamespace+"/build-b1"]["status"] = map[string]any{"phase": "Succeeded"}
	if err := s.DeleteImport(context.Background(), "b1"); err == nil {
		t.Fatal("replacement cleanup accepted", err)
	}
	if base.core["pods/"+BuildNamespace+"/build-b1"] == nil {
		t.Fatal("replacement import deleted")
	}
}
