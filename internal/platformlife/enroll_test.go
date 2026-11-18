package platformlife

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func enrollLineOf(payload string) string {
	return pve.EnrollPrefix + base64.StdEncoding.EncodeToString([]byte(payload))
}

// The pasted line: what the root command prints is read back, wrapped or not; anything else is refused without the
// line in the error, and a line of another installation says so.
func TestParseEnrollLine(t *testing.T) {
	const capi, csi = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	line := enrollLineOf("1:pi-0a1b2c3d:1795000000:" + capi + ":" + csi)
	wrapped := "  " + line[:40] + "\n" + line[40:90] + "\r\n " + line[90:] + "\n"
	for _, l := range []string{line, wrapped} {
		e, err := ParseEnrollLine(l, "pi-0a1b2c3d")
		if err != nil || e.Issued != 1795000000 || e.CAPI == nil || *e.CAPI != (TokenCredential{TokenID: pve.CAPITokenID, Secret: capi}) ||
			e.CSI == nil || *e.CSI != (TokenCredential{TokenID: pve.CSITokenID, Secret: csi}) {
			t.Fatalf("a good line: %+v, %v", e, err)
		}
		if s := e.String(); strings.Contains(s, capi) || strings.Contains(s, csi) {
			t.Fatalf("an enrollment prints its secrets: %s", s)
		}
	}
	// One token only: the other was kept by the command.
	if e, err := ParseEnrollLine(enrollLineOf("1:pi-0a1b2c3d:1795000000::"+csi), "pi-0a1b2c3d"); err != nil || e.CAPI != nil || e.CSI == nil {
		t.Fatalf("a line with one token: %+v, %v", e, err)
	}
	if _, err := ParseEnrollLine(line, "pi-ffffffff"); !errors.Is(err, ErrEnrollOther) {
		t.Fatalf("another installation: %v", err)
	}
	for name, l := range map[string]string{
		"empty":             "",
		"a bare secret":     capi,
		"no prefix":         strings.TrimPrefix(line, pve.EnrollPrefix),
		"cut short":         line[:len(line)-30],
		"not base64":        pve.EnrollPrefix + "!!!!",
		"another version":   enrollLineOf("2:pi-0a1b2c3d:1795000000:" + capi + ":" + csi),
		"a field too few":   enrollLineOf("1:pi-0a1b2c3d:1795000000:" + capi),
		"a field too many":  enrollLineOf("1:pi-0a1b2c3d:1795000000:" + capi + ":" + csi + ":x"),
		"no time":           enrollLineOf("1:pi-0a1b2c3d::" + capi + ":" + csi),
		"a negative time":   enrollLineOf("1:pi-0a1b2c3d:-5:" + capi + ":" + csi),
		"no installation":   enrollLineOf("1::1795000000:" + capi + ":" + csi),
		"no secret at all":  enrollLineOf("1:pi-0a1b2c3d:1795000000::"),
		"not a secret":      enrollLineOf("1:pi-0a1b2c3d:1795000000:hunter2:" + csi),
		"a secret cut":      enrollLineOf("1:pi-0a1b2c3d:1795000000:" + capi[:30] + ":" + csi),
		"the prefix inside": "x" + line,
	} {
		e, err := ParseEnrollLine(l, "pi-0a1b2c3d")
		if !errors.Is(err, ErrEnrollMalformed) || e != nil {
			t.Errorf("%s: %+v, %v", name, e, err)
		}
		if err != nil && (strings.Contains(err.Error(), capi) || strings.Contains(err.Error(), csi)) {
			t.Errorf("%s: the error quotes a secret: %v", name, err)
		}
	}
}
