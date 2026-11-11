package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLargeEnvAndIDPropagation(t *testing.T) {
	// Exercise init payload, start/ready userCmd and actual Linux exec with image ENV
	// plus >64 caller variables and a 16 KiB value. No credential values in failures.
	large := strings.Repeat("x", 16<<10)
	environ := []string{"PATH=/bin:/usr/bin", "IMAGE_VAR=retained", "E2B_SANDBOX_ID=sbx-real", "LARGE=" + large, "PROXCLOUD_ENVD_ACCESS_TOKEN=hidden"}
	for i := range 100 {
		environ = append(environ, fmt.Sprintf("V%d=value", i))
	}
	if err := validateBootEnv(environ); err != nil {
		t.Fatal(err)
	}
	user, child := splitEnv(environ)
	if user["E2B_SANDBOX_ID"] != "sbx-real" || user["IMAGE_VAR"] != "retained" || len(user["LARGE"]) != 16<<10 {
		t.Fatal("user env lost image/caller defaults or authoritative ID")
	}
	payload, err := initPayload(user, "token", "user", "/home/user", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		EnvVars map[string]string `json:"envVars"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.EnvVars["LARGE"] != large || decoded.EnvVars["E2B_SANDBOX_ID"] != "sbx-real" {
		t.Fatal("envd init round trip failed")
	}
	for _, kind := range []string{"start", "ready"} {
		cmd := userCmd("/bin/sh", []string{"-c", `test "$E2B_SANDBOX_ID" = sbx-real && test "${#LARGE}" = 16384 && test "$IMAGE_VAR" = retained`}, user, "root", "/tmp", "/tmp", 0, 0)
		cmd.Stdout, cmd.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s env propagation: %v", kind, err)
		}
	}
	// Check a near-128 KiB caller payload at the 32 KiB/value boundary, plus image
	// defaults. Linux counts NAME=value/NUL and pointers, not just value bytes.
	budgetEnv := append([]string{}, child...)
	for i := range 3 {
		budgetEnv = append(budgetEnv, fmt.Sprintf("BOUNDARY_%d=", i)+strings.Repeat("x", 32<<10))
	}
	if err := validateBootEnv(budgetEnv); err != nil {
		t.Fatal(err)
	}
	boundary := exec.Command("/bin/sh", "-c", `test "${#BOUNDARY_0}" = 32768 && test "${#BOUNDARY_2}" = 32768`)
	boundary.Env = budgetEnv
	if err := boundary.Run(); err != nil {
		t.Fatalf("near-max caller payload plus image ENV: %v", err)
	}
	// envd itself also starts with the merged large environment.
	cmd := exec.Command("/bin/true")
	cmd.Env = child
	if err := cmd.Run(); err != nil {
		t.Fatalf("merged child exec: %v", err)
	}
}
