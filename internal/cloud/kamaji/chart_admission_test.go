package kamaji

import (
	"strings"
	"testing"
)

// The portal renders no admissionControllers, so the chart's default is what every cluster runs, and it must carry
// NodeRestriction: Kamaji's own default leaves it out.
func TestChartEnablesNodeRestriction(t *testing.T) {
	out := renderChart(t, testSpec())
	_, cp, ok := strings.Cut(out, "\nkind: KamajiControlPlane\n")
	if !ok {
		t.Fatalf("no KamajiControlPlane rendered\n%s", out)
	}
	cp, _, _ = strings.Cut(cp, "\n---")
	if !strings.Contains(cp, `admissionControllers: [`) || !strings.Contains(cp, `"NodeRestriction"`) {
		t.Errorf("KamajiControlPlane lacks NodeRestriction\n%s", cp)
	}
}
