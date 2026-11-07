package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A restarted envd has no access token until /init: the sandbox must be not ready from before envd starts until /init
// succeeded, and stay not ready when /init fails.
func TestRestartEnvdUnreadyUntilInit(t *testing.T) {
	defer func(p string) { readyPath = p }(readyPath)
	for _, initErr := range []error{nil, errors.New("boom")} {
		readyPath = filepath.Join(t.TempDir(), "run", "ready")
		markReady()
		if readyCheck(readyPath) != 0 {
			t.Fatal("setup: marker missing")
		}
		release := make(chan struct{})
		done := make(chan struct{})
		restartEnvd(func() *exec.Cmd {
			if readyCheck(readyPath) == 0 {
				t.Error("envd started while the sandbox was still ready")
			}
			return &exec.Cmd{}
		}, func() error {
			<-release
			if readyCheck(readyPath) == 0 {
				t.Error("ready before /init finished")
			}
			defer close(done)
			return initErr
		}, 0)
		close(release)
		<-done
		time.Sleep(50 * time.Millisecond) // markReady runs right after reinit returns
		if got := readyCheck(readyPath) == 0; got != (initErr == nil) {
			t.Errorf("init err %v: ready=%v", initErr, got)
		}
	}
}
