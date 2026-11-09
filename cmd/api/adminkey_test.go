package main

import (
	"io"
	"testing"
)

// TestAdminKeyCmdArgs pins the subcommand's answers that need no database: help succeeds, a usage
// error and a missing database URL fail.
func TestAdminKeyCmdArgs(t *testing.T) {
	t.Setenv("PROXCLOUD_DB_URL", "")
	if err := adminKeyCmd([]string{"create", "-help"}, io.Discard); err != nil {
		t.Errorf("create -help: %v, want success", err)
	}
	for _, args := range [][]string{nil, {"list"}, {"--help"}, {"create"}} {
		if err := adminKeyCmd(args, io.Discard); err == nil {
			t.Errorf("%q: want an error", args)
		}
	}
}
