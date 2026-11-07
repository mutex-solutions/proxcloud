package pve

import (
	"strings"
	"testing"
)

func TestValidVMName(t *testing.T) {
	label := strings.Repeat("a", 63)
	for name, want := range map[string]bool{
		"my-server":                        true,
		"a":                                true,
		"9":                                true,
		"web1.example.org":                 true,
		"A-b-C":                            true,
		label:                              true,
		label + "a":                        false,
		strings.Repeat(label+".", 4) + "a": false,
		"":                                 false,
		"my server":                        false,
		"my_server":                        false,
		"-web":                             false,
		"web-":                             false,
		".web":                             false,
		"web.":                             false,
		"a..b":                             false,
		"web\n":                            false,
		"sérver":                           false,
		"a.-b":                             false,
	} {
		if got := ValidVMName(name); got != want {
			t.Errorf("%q: got %v want %v", name, got, want)
		}
	}
}
