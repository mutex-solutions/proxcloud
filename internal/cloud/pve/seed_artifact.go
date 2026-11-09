package pve

// Seed artifacts have their own write-ahead journal: a failed active-seed save,
// replaced CD or removed project must not make a plaintext file unaccountable.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

var ErrSeedCleanupPending = errors.New("seed cleanup pending")

// SeedArtifact contains identity and cleanup evidence, never seed payloads or credentials.
// Identity fields are immutable; only status/evidence fields are updated after Prepare.
type SeedArtifact struct {
	ID             string     `json:"id"`
	InstallationID string     `json:"installationId"`
	ServiceID      string     `json:"serviceId"`
	SeedID         string     `json:"seedId"`
	ProjectID      string     `json:"projectId"`
	ServerID       string     `json:"serverId"`
	InstanceID     string     `json:"instanceId"`
	Node           string     `json:"node"`
	Volume         string     `json:"volume"`
	Cluster        string     `json:"cluster"`
	Backing        string     `json:"backing"`
	CreatedAt      time.Time  `json:"createdAt"`
	State          string     `json:"state"`
	NoRefsAt       *time.Time `json:"noRefsAt,omitempty"`
	LastReason     string     `json:"lastReason,omitempty"`
	// Unknown mutation outcomes cannot be reclaimed automatically: a delayed
	// PVE worker might still attach/upload after an absence scan.
	MutationPending bool `json:"mutationPending,omitempty"`
}

// SeedArtifactRepo is services-side only. DB ownership plus an exact 128-bit
// random name is the authority; a file found by prefix is never adopted.
type SeedArtifactRepo struct {
	db                              *pgdoc.DB
	col, identity, pins, operations *pgdoc.Store
}

func NewSeedArtifactRepo(db *pgdoc.DB) *SeedArtifactRepo {
	return &SeedArtifactRepo{db: db, col: db.C("pveSeedArtifact"), identity: db.C("pveSeedIdentity"), pins: db.C("pveSeedRecoveryPin"), operations: db.C("pveSeedOperation")}
}

type seedLockKey struct{}

func (r *SeedArtifactRepo) Locked(ctx context.Context, service string, fn func(context.Context) error) error {
	if ctx.Value(seedLockKey{}) == service {
		return fn(ctx)
	}
	if err := r.operations.Ensure(ctx); err != nil {
		return err
	}
	_, err := r.db.WithAdvisoryLock(ctx, "pve-seed:"+service, false, func() error {
		// A session lock can disappear with its connection while PVE work is
		// still running. A non-expiring durable claim prevents a new holder
		// entering until this exact callback ends or an operator reconciles it.
		claim, err := seedRandomID()
		if err != nil {
			return err
		}
		err = r.db.WithTx(ctx, func(ctx context.Context) error {
			if err := r.db.LockXact(ctx, "pve-seed-claim:"+service); err != nil {
				return err
			}
			var old pgdoc.M
			found, err := r.operations.Get(ctx, service, &old)
			if err != nil {
				return err
			}
			if found {
				return fmt.Errorf("%w: unfinished seed operation requires reconciliation", ErrSeedCleanupPending)
			}
			return r.operations.Upsert(ctx, service, pgdoc.M{"claim": claim, "serviceId": service, "startedAt": time.Now().UTC(), "state": "running-or-reconciliation-required"})
		})
		if err != nil {
			return err
		}
		result := fn(context.WithValue(ctx, seedLockKey{}, service))
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = r.db.WithTx(cleanup, func(ctx context.Context) error {
			if err := r.db.LockXact(ctx, "pve-seed-claim:"+service); err != nil {
				return err
			}
			var row pgdoc.M
			found, err := r.operations.Get(ctx, service, &row)
			if err != nil {
				return err
			}
			if !found || row["claim"] != claim {
				return fmt.Errorf("seed operation claim changed")
			}
			_, err = r.operations.DeleteByID(ctx, service)
			return err
		})
		if err != nil {
			return fmt.Errorf("%w: seed operation completion requires reconciliation", ErrSeedCleanupPending)
		}
		return result
	})
	return err
}

func seedRandomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func (r *SeedArtifactRepo) installation(ctx context.Context) (string, error) {
	var out struct {
		ID    string `json:"id"`
		Owner string `json:"owner"`
	}
	if err := r.identity.Ensure(ctx); err != nil {
		return "", err
	}
	err := r.db.WithTx(ctx, func(ctx context.Context) error {
		if err := r.db.LockXact(ctx, "pve-seed-installation"); err != nil {
			return err
		}
		found, err := r.identity.Get(ctx, "installation", &out)
		if err != nil {
			return err
		}
		if found {
			if !seedIDRE.MatchString(out.Owner) {
				return fmt.Errorf("invalid seed installation identity")
			}
			return nil
		}
		out.ID = "installation"
		out.Owner, err = seedRandomID()
		if err != nil {
			return err
		}
		_, err = r.identity.InsertOne(ctx, &out)
		return err
	})
	return out.Owner, err
}

var seedIDRE = regexp.MustCompile(`^[a-f0-9]{32}$`)
var ownedSeedRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]*):iso/(pcseed-v1-([a-f0-9]{32})-([a-f0-9]{32})\.iso)$`)

func ownedSeedVolume(vol string) (store, name, installation, id string, ok bool) {
	m := ownedSeedRE.FindStringSubmatch(vol)
	if m == nil {
		return "", "", "", "", false
	}
	return m[1], m[2], m[3], m[4], true
}

// Prepare commits the exact filename before any PVE upload. Backing identity
// is captured now, so remapping the storage cannot authorize deleting a new file.
func (r *SeedArtifactRepo) Prepare(ctx context.Context, c *Client, a SeedArtifact, storage string) (*SeedArtifact, error) {
	owner, err := r.installation(ctx)
	if err != nil {
		return nil, err
	}
	id, err := seedRandomID()
	if err != nil {
		return nil, err
	}
	a.ID = id
	a.InstallationID = owner
	a.Volume = storage + ":iso/pcseed-v1-" + owner + "-" + id + ".iso"
	if _, _, _, _, ok := ownedSeedVolume(a.Volume); !ok {
		return nil, fmt.Errorf("invalid seed storage")
	}
	// Insufficient identity read must not break a formerly working create.
	// Missing identity only removes automatic cleanup authority, never journaling.
	a.Cluster, _ = c.seedClusterIdentity(ctx)
	a.Backing, _ = c.seedBacking(ctx, a.Node, storage)
	a.CreatedAt = time.Now().UTC()
	a.State = "prepared"
	_, err = r.col.InsertOne(ctx, &a)
	return &a, err
}
func (r *SeedArtifactRepo) artifact(ctx context.Context, service, volume string) (*SeedArtifact, error) {
	_, _, owner, id, ok := ownedSeedVolume(volume)
	if !ok {
		return nil, fmt.Errorf("%w: legacy or invalid seed filename; operator inspection required", ErrSeedCleanupPending)
	}
	var identity struct {
		Owner string `json:"owner"`
	}
	found, err := r.identity.Get(ctx, "installation", &identity)
	if err != nil {
		return nil, err
	}
	if !found || identity.Owner != owner {
		return nil, fmt.Errorf("%w: seed installation ownership is not proven", ErrSeedCleanupPending)
	}
	var a SeedArtifact
	found, err = r.col.Get(ctx, id, &a)
	if err != nil {
		return nil, err
	}
	if !found || a.ID != id || a.InstallationID != owner || a.ServiceID != service || a.Volume != volume {
		return nil, fmt.Errorf("%w: no matching seed artifact journal entry", ErrSeedCleanupPending)
	}
	return &a, nil
}
func (r *SeedArtifactRepo) status(ctx context.Context, a *SeedArtifact, state, reason string, absent *time.Time) error {
	_, err := r.col.SetByID(ctx, a.ID, pgdoc.M{"state": state, "lastReason": reason, "noRefsAt": absent}, nil)
	return err
}
func (r *SeedArtifactRepo) mutation(ctx context.Context, a *SeedArtifact, pending bool) error {
	_, err := r.col.SetByID(ctx, a.ID, pgdoc.M{"mutationPending": pending}, nil)
	return err
}
func (r *SeedArtifactRepo) Attached(ctx context.Context, a *SeedArtifact) error {
	if err := r.mutation(ctx, a, false); err != nil {
		return err
	}
	return r.status(ctx, a, "attached", "", nil)
}
func (r *SeedArtifactRepo) Pending(ctx context.Context, service string) ([]SeedArtifact, error) {
	out := []SeedArtifact{}
	err := r.col.Find(ctx, pgdoc.M{"serviceId": service, "state": pgdoc.M{"$nin": []string{"deleted", "emptied"}}}, &out)
	return out, err
}

// ServerMutationUnknown vetoes every generation while any asynchronous seed
// mutation on this VMID may still finish. No expiry guesses task completion.
func (r *SeedArtifactRepo) ServerMutationUnknown(ctx context.Context, service, server string) (bool, error) {
	var a SeedArtifact
	return r.col.FindOne(ctx, pgdoc.M{"serviceId": service, "serverId": server, "mutationPending": true, "state": pgdoc.M{"$nin": []string{"deleted", "emptied"}}}, &a)
}

// PinRecovery commits before a backup or restore can resurrect an older CD.
// Intentionally persists across VMID reuse and DELETE_BACKUP: clearing requires
// operator proof that all archives and in-flight tasks are gone, not a local list.
func (r *SeedArtifactRepo) PinRecovery(ctx context.Context, service, server string) error {
	return r.pins.Upsert(ctx, service+":"+server, pgdoc.M{"serviceId": service, "serverId": server, "reason": "backup/restore may reference a previous seed; operator archive/task reconciliation required"})
}
func (r *SeedArtifactRepo) recoveryPinned(ctx context.Context, service, server string) (bool, error) {
	var pin pgdoc.M
	// An archive of another VM may name this ISO (cross-VM CD attachments are
	// valid PVE). Until archives are inventoried, any recovery pin fences the
	// entire provider rather than guessing its reference set from a VMID.
	return r.pins.FindOne(ctx, pgdoc.M{"serviceId": service}, &pin)
}

// ForSeed includes every generation and superseded file. Active ServerSeed.ISO
// can be empty/old after a crash, so it is never the cleanup authority by itself.
func (r *SeedArtifactRepo) ForSeed(ctx context.Context, service, seedID string) ([]SeedArtifact, error) {
	out := []SeedArtifact{}
	err := r.col.Find(ctx, pgdoc.M{"serviceId": service, "seedId": seedID}, &out)
	return out, err
}

// SeedCleanupReport is an operator-facing, secret-free read of durable cleanup
// status. Attached files are separate: they still hold the live seed by design.
func (r *SeedArtifactRepo) Report(ctx context.Context, service string) (map[string]any, error) {
	var all []SeedArtifact
	if err := r.col.Find(ctx, pgdoc.M{"serviceId": service}, &all); err != nil {
		return nil, err
	}
	pending := []SeedArtifact{}
	attached, emptied, deleted := 0, 0, 0
	for _, a := range all {
		switch a.State {
		case "attached":
			attached++
		case "deleted":
			deleted++
		case "emptied":
			emptied++
		default:
			pending = append(pending, a)
		}
	}
	var pins []pgdoc.M
	if err := r.pins.Find(ctx, pgdoc.M{"serviceId": service}, &pins); err != nil {
		return nil, err
	}
	var operation pgdoc.M
	_, err := r.operations.Get(ctx, service, &operation)
	if err != nil {
		return nil, err
	}
	delete(operation, "claim") // claim is not a credential, but no caller needs the fencing nonce.
	return map[string]any{"operation": operation, "attached": attached, "pendingCount": len(pending), "emptied": emptied, "deleted": deleted, "pending": pending, "recoveryPins": pins, "warning": "Live seed CDs retain plaintext user-data; incomplete reference visibility blocks removal. Legacy files are not adopted."}, nil
}
