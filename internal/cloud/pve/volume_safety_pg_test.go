//go:build integration

package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// A lease can be acquired/released for a refused admission check, but there must
// be no committed R1 mutation intent (not merely no visible pending operation).
func TestVolumeSafetyRefusalHasNoPostgresIntent(t *testing.T) {
	db := holderPG(t)
	ctx := context.Background()
	store := NewHolderOperationRepo(db)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"extend", "root resize", "retype"} {
		t.Run(operation, func(t *testing.T) {
			writes := map[string][]url.Values{}
			s := volCluster(t, cfgAttached, writes, fakeRoutes{
				"GET " + safetyVM + "/pending":        say(`{"data":[]}`),
				"GET " + safetyVM + "/status/current": say(`{"data":{"status":"running"}}`),
				"GET " + safetyVM + "/snapshot":       say(`{"data":[{"name":"before","snaptime":1794222000},{"name":"current","parent":"before"}]}`),
				"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("type") == "storage" {
						fmt.Fprint(w, `{"data":[{"type":"storage","storage":"local-zfs","node":"pve1","status":"available","maxdisk":107374182400,"disk":106300440576}]}`)
						return
					}
					fmt.Fprint(w, `{"data":[{"type":"qemu","vmid":101,"node":"pve1","pool":"tp-p1"}]}`)
				},
			})
			s.ServiceID, s.HolderOperations = "svc", store
			var err error
			switch operation {
			case "extend":
				err = s.ExtendVolume(ctx, "p1", 101, "local-zfs:vm-101-disk-1", 40, 10)
			case "root resize":
				err = s.ResizeServer(ctx, "p1", "101", ServerSpec{DiskGb: 40, Slug: "large", VCPUs: 2, RAMMb: 1024}, 10)
			case "retype":
				err = s.RetypeVolume(ctx, "p1", 101, "local-zfs:vm-101-disk-1", "nas", 10)
			}
			var ce *ConflictError
			if !errors.As(err, &ce) || len(writes) != 0 {
				t.Fatalf("err=%v PVE writes=%v", err, writes)
			}
			var intents int
			if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM "pveHolderOperation"`).Scan(&intents); err != nil || intents != 0 {
				t.Fatalf("committed mutation intents=%d err=%v", intents, err)
			}
		})
	}
}
