package main

import (
	"log/slog"
	"testing"
)

// Most packages log through slog's package functions. Unless the process's JSON logger is slog's default too, their
// lines come out as plain text, which a filter on the JSON "level" field never sees (production, 2026-11-17: the
// three ERROR lines behind every failed Kubernetes create).
func TestNewLoggerIsSlogDefault(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	if log := newLogger("INFO"); slog.Default() != log {
		t.Fatal("slog's default logger is not the process's JSON logger")
	}
}
