package platformlife

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// Every code a run can hand to the job store passes the store's own rule (durablejobs.ValidCode, the check inside
// Store.Fail and Store.Defer): every Code… constant of this package (a stage ends a run with one of them), the
// deferral code of every stage id (waiting_<id> and retry_<id>, runner.go), and the three literals of the runner. The
// constants are read from the source, so a new code or stage cannot be left out. The store refused the digit of
// waiting_seed_k3s and seed_k3s_failed until 0.10.1-rc.2: the stage could neither defer nor record its failure.
func TestEveryCodePassesTheJobStore(t *testing.T) {
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	codes, stages := map[string]string{}, map[string]string{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				vs, ok := n.(*ast.ValueSpec)
				if !ok {
					return true
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, _ := strconv.Unquote(lit.Value)
					switch {
					case strings.HasPrefix(name.Name, "Code"):
						codes[name.Name] = v
					case strings.HasPrefix(name.Name, "Stage") || strings.HasPrefix(name.Name, "stage"):
						stages[name.Name] = v
					}
				}
				return true
			})
		}
	}
	if len(codes) < 40 || stages["stageSeedK3s"] != "seed-k3s" || codes["CodeSeedK3sFailed"] == "" {
		t.Fatalf("the source was not read: %d codes, %d stage constants", len(codes), len(stages))
	}
	for name, code := range codes {
		if !durablejobs.ValidCode(code) {
			t.Errorf("%s = %q: the job store refuses it, so a run that ends with it cannot record its failure", name, code)
		}
	}
	// Every id a run's stage list can hold, whatever constant names it.
	ids := map[string]bool{}
	for _, kind := range []string{RunApply, RunRemove} {
		for _, part := range []string{"", PartSeed} {
			for _, id := range stageIDs(kind, part) {
				ids[id] = true
			}
		}
	}
	for _, id := range stages {
		ids[id] = true
	}
	for id := range ids {
		if code := TimeoutCode(id); !durablejobs.ValidCode(code) {
			t.Errorf("stage %s: the job store refuses its deadline code %q", id, code)
		}
		for _, prefix := range []string{"waiting_", "retry_"} {
			if code := prefix + strings.ReplaceAll(id, "-", "_"); !durablejobs.ValidCode(code) {
				t.Errorf("stage %s: the job store refuses %q, so the stage cannot give its job back", id, code)
			}
		}
	}
	for _, code := range []string{"shutdown", "retryable", "invalid_payload"} {
		if !durablejobs.ValidCode(code) {
			t.Errorf("%q refused", code)
		}
	}
}
