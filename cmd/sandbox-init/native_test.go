package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeExecPreservesCommand(t *testing.T) {
	if os.Getenv("NATIVE_TEST_CHILD") == "1" {
		if err := nativeExecAt([]string{"sh", "-c", `test "$PAYLOAD" = 'a b' && test "$1" = 'literal;$x' && test -s "$MARKER" && printf 'kept' > notes.txt; exit 17`, "sh", "literal;$x"}, os.Getenv("MARKER")); err != nil {
			panic(err)
		}
		return
	}
	root := t.TempDir()
	marker := filepath.Join(root, "boot")
	c := exec.Command(os.Args[0], "-test.run=^TestNativeExecPreservesCommand$")
	c.Dir = root
	c.Env = append(os.Environ(), "NATIVE_TEST_CHILD=1", "PAYLOAD=a b", "MARKER="+marker)
	out, err := c.CombinedOutput()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 17 {
		t.Fatalf("exit/env/argv: %v %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil || string(b) != "kept" {
		t.Fatalf("workdir/file: %q %v", b, err)
	}
	b, err = os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err != nil {
		t.Fatal(err)
	}
}

func TestNativeModeRequiresPID1AndCommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		pid  int
		ok   bool
	}{
		{[]string{"-native", "sleep", "infinity"}, 1, true},
		{[]string{"-native", "sleep", "infinity"}, 42, false},
		{[]string{"-native"}, 1, false},
		{[]string{"-snapshot", "ignored"}, 42, false},
	} {
		_, err := runMode(tc.args, tc.pid)
		if (err == nil) != tc.ok {
			t.Fatalf("%v pid=%d: %v", tc.args, tc.pid, err)
		}
	}
}
