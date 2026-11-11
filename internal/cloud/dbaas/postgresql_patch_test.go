package dbaas

import (
	"os"
	"os/exec"
	"testing"
)

// TestPostgreSQLPatchImages renders the real chart via the existing Helm helper:
// provider-facing versions remain majors, while CNPG receives a same-major patch
// image. Map coverage for 15/16 does not add them to the fresh installer catalog.
func TestPostgreSQLPatchImages(t *testing.T) {
	// renderChart skips without Helm locally. In CI this regression must render,
	// not silently pass without checking the actual template/image contract.
	if os.Getenv("CI") != "" {
		if _, err := exec.LookPath("helm"); err != nil {
			t.Fatalf("PostgreSQL chart image checks require Helm in CI: %v", err)
		}
	}

	for _, tc := range []struct {
		major string
		image string
	}{
		{"15", "ghcr.io/cloudnative-pg/postgresql:15.19"},
		{"16", "ghcr.io/cloudnative-pg/postgresql:16.15"},
		{"17", "ghcr.io/cloudnative-pg/postgresql:17.11"},
		{"18", "ghcr.io/cloudnative-pg/postgresql:18.6"},
	} {
		t.Run(tc.major, func(t *testing.T) {
			spec := testSpec(EnginePostgreSQL, tc.major)
			values := BuildValues(testConfig(), spec)
			if got := values["engineVersion"]; got != tc.major {
				t.Fatalf("engineVersion = %v, want unchanged major %q", got, tc.major)
			}

			var cluster map[string]any
			for _, doc := range renderChart(t, values) {
				if doc["apiVersion"] != "postgresql.cnpg.io/v1" || doc["kind"] != "Cluster" {
					continue
				}
				if got := digStr(doc, "metadata", "name"); got != spec.ID {
					t.Fatalf("CNPG Cluster name = %q, want %q", got, spec.ID)
				}
				if cluster != nil {
					t.Fatal("more than one CNPG Cluster rendered")
				}
				cluster = doc
			}
			if cluster == nil {
				t.Fatal("no CNPG Cluster rendered")
			}
			if got := digStr(cluster, "spec", "imageName"); got != tc.image {
				t.Errorf("PostgreSQL %s imageName = %q, want %q", tc.major, got, tc.image)
			}
		})
	}
}
