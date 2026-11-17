package project

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// F51: the CD a live server is seeded from is not a cleanup candidate; a superseded one, a retired or stale server's
// and one no seed row names are.
func TestSeedCleanupCandidatesLeaveLiveServersAlone(t *testing.T) {
	rows := []ServerSeed{
		{ID: "live", ISO: "cephfs:iso/live.iso"},
		{ID: "retired", ISO: "cephfs:iso/retired.iso", Retired: true},
		{ID: "stale", ISO: "cephfs:iso/stale.iso"},
	}
	artifacts := []pve.SeedArtifact{{ID: "a", Volume: "cephfs:iso/live.iso", State: "attached"}, {ID: "b", Volume: "cephfs:iso/live-old.iso"},
		{ID: "c", Volume: "cephfs:iso/retired.iso"}, {ID: "d", Volume: "cephfs:iso/stale.iso"}}
	got := seedCleanupCandidates(artifacts, rows, rows[2:])
	if len(got) != 3 || got[0].ID != "b" || got[1].ID != "c" || got[2].ID != "d" {
		t.Fatalf("candidates %+v", got)
	}
}
