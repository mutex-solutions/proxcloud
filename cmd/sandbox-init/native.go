package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Native mode does not run envd, create users, change uid/workdir/environment or
// interpret a shell string. Replacing PID 1 preserves the original command's
// signal/exit behavior. /tmp permits images with a non-root USER to mark boot.
const nativeBootPath = "/tmp/.proxcloud-native-boot"

func nativeExec(argv []string) error { return nativeExecAt(argv, nativeBootPath) }

func nativeExecAt(argv []string, marker string) error {
	if len(argv) == 0 {
		return fmt.Errorf("native command required")
	}
	if err := validateBootEnv(os.Environ()); err != nil {
		return fmt.Errorf("environment: %w", err)
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		return fmt.Errorf("native boot marker (requires writable /tmp): %w", err)
	}
	return syscall.Exec(bin, argv, os.Environ())
}
