package config

import (
	"go/version"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	supportedGoBuildVersion  = "1.27.4"
	supportedGoModuleMinimum = "1.26.0"
)

// TestSupportedGoVersionPolicy keeps the selected stable compiler and the
// dependency-driven module minimum explicit, valid, and intentionally distinct.
func TestSupportedGoVersionPolicy(t *testing.T) {
	if supportedGoBuildVersion != "1.27.4" {
		t.Errorf("build compiler = %q; want selected stable release 1.27.4", supportedGoBuildVersion)
	}
	if supportedGoModuleMinimum != "1.26.0" {
		t.Errorf("module minimum = %q; want dependency-driven minimum 1.26.0", supportedGoModuleMinimum)
	}

	compiler := "go" + supportedGoBuildVersion
	minimum := "go" + supportedGoModuleMinimum
	if !version.IsValid(compiler) || !version.IsValid(minimum) {
		t.Fatalf("invalid Go version policy: compiler %q, minimum %q", compiler, minimum)
	}
	if version.Compare(minimum, compiler) > 0 {
		t.Errorf("module minimum %s exceeds build compiler %s", minimum, compiler)
	}
	if minimum == compiler {
		t.Error("module minimum must remain separate from the exact build compiler patch")
	}
}

// TestSupportedGoBuildBaseline checks source declarations only. It does not build
// any binary, inspect artifact provenance, or cover the external envd module.
func TestSupportedGoBuildBaseline(t *testing.T) {
	root := goBuildBaselineRoot(t)
	image := "golang:" + supportedGoBuildVersion + "-alpine"
	builders := []struct {
		path   string
		stages map[string]string
	}{
		{"deploy/Dockerfile", map[string]string{"build": image}},
		{"deploy/sandbox-proxy.Dockerfile", map[string]string{"build": image}},
		{"deploy/sandbox-import.Dockerfile", map[string]string{"build": image}},
		{"deploy/sandbox-runtime.Dockerfile", map[string]string{"envd": image, "init": image}},
		{"deploy/templates/mcp-gateway/Dockerfile", map[string]string{"gateway": "docker.io/library/" + image}},
	}
	from := regexp.MustCompile(`(?im)^[\t ]*FROM[\t ]+(?:--platform=\S+[\t ]+)?(\S+)(?:[\t ]+AS[\t ]+([^\t \r\n#]+))?`)
	for _, builder := range builders {
		t.Run(builder.path, func(t *testing.T) {
			source := goBuildBaselineRead(t, root, builder.path)
			seen := make(map[string]bool)
			goStages := 0
			for _, declaration := range from.FindAllStringSubmatch(source, -1) {
				image, stage := declaration[1], strings.ToLower(declaration[2])
				base := image[strings.LastIndex(image, "/")+1:]
				isGo := base == "golang" || strings.HasPrefix(base, "golang:") || strings.HasPrefix(base, "golang@")
				want, expected := builder.stages[stage]
				if isGo {
					goStages++
					if !expected {
						t.Errorf("unexpected Go builder stage %q using %q", stage, image)
					}
				}
				if !expected {
					continue
				}
				if seen[stage] {
					t.Errorf("duplicate builder stage %q", stage)
				}
				seen[stage] = true
				if image != want {
					t.Errorf("stage %q uses %q; want exact %q", stage, image, want)
				}
			}
			for stage := range builder.stages {
				if !seen[stage] {
					t.Errorf("missing builder stage %q", stage)
				}
			}
			if goStages != len(builder.stages) {
				t.Errorf("found %d Go stages; want %d", goStages, len(builder.stages))
			}
		})
	}

	t.Run("CI compiler", func(t *testing.T) {
		var workflow struct {
			Jobs map[string]struct {
				Steps []struct {
					Uses string `yaml:"uses"`
					With struct {
						GoVersion     string `yaml:"go-version"`
						GoVersionFile string `yaml:"go-version-file"`
					} `yaml:"with"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		source := goBuildBaselineRead(t, root, ".github/workflows/test.yml")
		if err := yaml.Unmarshal([]byte(source), &workflow); err != nil {
			t.Fatal(err)
		}
		setups := 0
		for job, config := range workflow.Jobs {
			for _, step := range config.Steps {
				if !strings.HasPrefix(step.Uses, "actions/setup-go@") {
					continue
				}
				setups++
				if step.With.GoVersion != supportedGoBuildVersion || step.With.GoVersionFile != "" {
					t.Errorf("job %q must select exact Go %s, not %q or version file %q", job, supportedGoBuildVersion, step.With.GoVersion, step.With.GoVersionFile)
				}
			}
		}
		if setups == 0 {
			t.Fatal("test workflow has no actions/setup-go compiler selection")
		}
	})

	t.Run("module minimum is separate", func(t *testing.T) {
		source := goBuildBaselineRead(t, root, "go.mod")
		minimum := regexp.MustCompile(`(?m)^go[\t ]+([^\r\n]+)`).FindAllStringSubmatch(source, -1)
		if len(minimum) != 1 || strings.TrimSpace(minimum[0][1]) != supportedGoModuleMinimum {
			t.Fatalf("keep root module minimum go %s separate from the build compiler pin", supportedGoModuleMinimum)
		}
	})
}

func goBuildBaselineRoot(t *testing.T) string {
	t.Helper()
	// Go runs this package's tests from internal/config; verify that ../../ is
	// this repository rather than silently reading another checkout's files.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	module := goBuildBaselineRead(t, root, "go.mod")
	if !strings.HasPrefix(module, "module github.com/mutex-solutions/proxcloud\n") {
		t.Fatalf("unexpected repository root %q", root)
	}
	return root
}

func goBuildBaselineRead(t *testing.T, root, path string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read baseline source %s: %v", path, err)
	}
	return string(content)
}
