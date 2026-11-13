package templateupdates

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// Opt-in REAL PostgreSQL (TEMPLATEUPDATES_TEST_POSTGRES): the sandbox kind is an external kind of the store (claimed
// only by kind, never by the DB-only Claim, re-acquirable dead), and the pin/receipt tables round-trip.
func TestPGSandboxKindAndStore(t *testing.T) {
	db, jobs := pgFixture(t)
	ctx := context.Background()
	store := PGSandbox{DB: db}
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	p := sbxPayload()
	raw, _ := json.Marshal(p)
	var id string
	if err := db.WithTx(ctx, func(tc context.Context) error {
		var err error
		if id, err = jobs.EnqueueTx(tc, durablejobs.TemplateUpdateSandbox, p.Key(), raw, 3); err != nil {
			return err
		}
		return store.Save(tc, &SandboxReceipt{ID: id, Kind: "sandbox", Payload: p, Step: SbxQueued, CreatedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
	// The DB-only worker's Claim never takes it; ClaimKinds of the VM kind neither.
	if j, err := jobs.Claim(ctx, time.Minute); err != nil || j != nil {
		t.Fatalf("DB-only claim took a sandbox job: %v %v", j, err)
	}
	if j, err := jobs.ClaimKinds(ctx, time.Minute, []string{durablejobs.TemplateUpdateVM}); err != nil || j != nil {
		t.Fatalf("VM runner took a sandbox job: %v %v", j, err)
	}
	j, err := jobs.ClaimKinds(ctx, time.Minute, []string{durablejobs.TemplateUpdateSandbox})
	if err != nil || j == nil || j.ID != id {
		t.Fatalf("claim: %v %v", j, err)
	}
	// Dies: listed for cleanup, re-acquirable fenced.
	if err := jobs.Fail(ctx, j, 0, true, "digest_mismatch"); err != nil {
		t.Fatal(err)
	}
	if ids, err := store.DeadUncleaned(ctx); err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("dead uncleaned %v %v", ids, err)
	}
	d, err := jobs.ClaimDead(ctx, id, time.Minute)
	if err != nil || d == nil || d.Epoch <= j.Epoch || d.Attempts != d.MaxAttempts {
		t.Fatalf("claim dead %+v %v", d, err)
	}
	if err := jobs.Checkpoint(ctx, j, nil); err == nil {
		t.Fatal("the first acquisition still passes the fence")
	}
	// Pins round-trip and filter by registry.
	if err := store.SavePin(ctx, &SandboxPin{ID: p.Repo(), Template: "base", Registry: sbxReg, Digest: sbxNew, Ref: p.Ref(), At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePin(ctx, &SandboxPin{ID: "198.51.100.1/library/sandbox-x", Template: "x", Registry: "198.51.100.1", Digest: sbxOld}); err != nil {
		t.Fatal(err)
	}
	if pins, err := store.Pins(ctx, sbxReg); err != nil || len(pins) != 1 || pins[0].Digest != sbxNew {
		t.Fatalf("pins %v %v", pins, err)
	}
	if all, _ := store.Pins(ctx, ""); len(all) != 2 {
		t.Fatalf("all pins %v", all)
	}
	if recs, err := store.ByRepo(ctx, sbxReg, "base"); err != nil || len(recs) != 1 {
		t.Fatalf("by repo %v %v", recs, err)
	}
}
