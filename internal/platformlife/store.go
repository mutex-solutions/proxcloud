package platformlife

// store.go — the three collections of the platform lifecycle: platformInstallation (§3.2), platformSecret (§3.3,
// the internal/cloud/cephcred pattern: values sealed with the shared data key, kept out of the installation so no read
// can return them) and platformRun (§3.4).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// Collections.
const (
	InstallationCollection = "platformInstallation"
	SecretCollection       = "platformSecret"
	RunCollection          = "platformRun"
	ProbeCollection        = "platformTenantProbe"
)

var (
	ErrNotFound        = errors.New("no managed-services installation with that id")
	ErrDuplicate       = errors.New("this Proxmox provider has a managed-services installation already")
	ErrRevConflict     = errors.New("the installation changed since it was read: read it again")
	ErrRunActive       = errors.New("a run of this installation is not finished: wait for it or cancel it")
	ErrNoEncryptionKey = errors.New("no data encryption key is configured (PROXCLOUD_ENCRYPTION_DEFAULT_KEY), so platform credentials are not stored")
)

// Secrets is one platformSecret document (id = the installation's). No route returns it and it is never passed to
// audit.RecordSnapshots; every secret value is sealed at rest.
type Secrets struct {
	ID             string           `json:"id"`
	CAPI           *TokenCredential `json:"capi,omitempty"` // capmox@pve!capi
	CSI            *TokenCredential `json:"csi,omitempty"`  // csi@pve!csi
	K3sToken       string           `json:"k3sToken,omitempty"`
	SeedKubeconfig *SeedKubeconfig  `json:"seedKubeconfig,omitempty"`
	BootstrapToken string           `json:"bootstrapToken,omitempty"` // wiped once the seed's identity is stored
	UpdatedAt      time.Time        `json:"updatedAt"`
}

// TokenCredential is a PVE API token: its id is not secret, its secret is.
type TokenCredential struct {
	TokenID string `json:"tokenId"`
	Secret  string `json:"secret"`
}

// SeedKubeconfig is the seed's service-account credential: server and CA are public, the token is not.
type SeedKubeconfig struct {
	Server string `json:"server"`
	CA     string `json:"ca"`
	Token  string `json:"token"`
}

// Store reads and writes the three collections.
type Store struct {
	DB  *pgdoc.DB
	enc *textcrypt.Encryptor
}

// NewStore returns a store sealing secrets with enc (the shared data key, as cephcred).
func NewStore(db *pgdoc.DB, enc *textcrypt.Encryptor) *Store { return &Store{DB: db, enc: enc} }

func (s *Store) installations() *pgdoc.Store { return s.DB.C(InstallationCollection) }
func (s *Store) secrets() *pgdoc.Store       { return s.DB.C(SecretCollection) }
func (s *Store) runs() *pgdoc.Store          { return s.DB.C(RunCollection) }
func (s *Store) probes() *pgdoc.Store        { return s.DB.C(ProbeCollection) }

// Probe is the installation's last tenant path test, nil when none was run.
func (s *Store) Probe(ctx context.Context, id string) (*TenantProbe, error) {
	var p TenantProbe
	found, err := s.probes().Get(ctx, id, &p)
	if err != nil || !found {
		return nil, err
	}
	return &p, nil
}

// SaveProbe replaces the installation's tenant path test.
func (s *Store) SaveProbe(ctx context.Context, p *TenantProbe) error {
	return s.probes().Upsert(ctx, p.ID, p)
}

// Ensure creates the tables and indexes at startup (writes run inside transactions, where pgdoc cannot auto-create a
// table). serviceId is unique: one installation per provider.
func (s *Store) Ensure(ctx context.Context) error {
	for _, c := range []*pgdoc.Store{s.installations(), s.secrets(), s.runs(), s.probes()} {
		if err := c.Ensure(ctx); err != nil {
			return err
		}
	}
	if err := s.installations().EnsureIndex(ctx, "serviceId", true, pgdoc.F("serviceId")); err != nil {
		return err
	}
	return s.runs().EnsureIndex(ctx, "installationId", false, pgdoc.F("installationId"))
}

// Create stores a new installation in phase draft with a fresh id.
func (s *Store) Create(ctx context.Context, in *Installation) error {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	now := time.Now().UTC()
	in.ID, in.CreatedAt, in.UpdatedAt, in.Rev = "pi-"+hex.EncodeToString(b[:]), now, now, 1
	in.Status = Status{Phase: PhaseDraft}
	if _, err := s.installations().InsertOne(ctx, in); pgdoc.IsDup(err) {
		return ErrDuplicate
	} else if err != nil {
		return err
	}
	return nil
}

// Get reads one installation (ErrNotFound when absent).
func (s *Store) Get(ctx context.Context, id string) (*Installation, error) {
	var in Installation
	found, err := s.installations().Get(ctx, id, &in)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return &in, nil
}

// HasInstallation reports whether a provider carries an installation, in any phase.
func (s *Store) HasInstallation(ctx context.Context, serviceID string) (bool, error) {
	return s.installations().Exists(ctx, pgdoc.M{"serviceId": serviceID})
}

// List reads every installation, oldest first.
func (s *Store) List(ctx context.Context) ([]Installation, error) {
	out := []Installation{}
	err := s.installations().Find(ctx, pgdoc.M{}, &out, pgdoc.Sort(pgdoc.AscK("createdAt", pgdoc.KTime)))
	return out, err
}

// Update replaces an installation with what change makes of it, under the lock a run start takes
// ("platform-run:"+id): it refuses when the stored rev is not rev or a run is not finished. It returns the
// installation before and after.
func (s *Store) Update(ctx context.Context, id string, rev int64, change func(old Installation) (*Installation, error)) (before, after *Installation, err error) {
	err = s.DB.WithTx(ctx, func(ctx context.Context) error {
		if err := s.DB.LockXact(ctx, "platform-run:"+id); err != nil {
			return err
		}
		var old Installation
		found, err := s.installations().GetForUpdate(ctx, id, &old)
		switch {
		case err != nil:
			return err
		case !found:
			return ErrNotFound
		case old.Rev != rev:
			return ErrRevConflict
		}
		if run, err := s.ActiveRun(ctx, id); err != nil {
			return err
		} else if run != nil {
			return ErrRunActive
		}
		next, err := change(old)
		if err != nil {
			return err
		}
		next.Rev, next.UpdatedAt = old.Rev+1, time.Now().UTC()
		if _, err := s.installations().Replace(ctx, id, next); err != nil {
			return err
		}
		before, after = &old, next
		return nil
	})
	return before, after, err
}

// SaveCredentials replaces the CAPI and the CSI token of an installation; a nil one is kept. Nothing is stored
// without a data key: the textcrypt encryptor would keep the secret in plain text. Every write of platformSecret
// rewrites the whole document, so each takes the installation's run lock before it reads.
func (s *Store) SaveCredentials(ctx context.Context, id string, capi, csi *TokenCredential) error {
	if !s.enc.HasKey() {
		return ErrNoEncryptionKey
	}
	return s.DB.WithTx(ctx, func(ctx context.Context) error {
		if err := s.DB.LockXact(ctx, "platform-run:"+id); err != nil {
			return err
		}
		sec, err := s.Secrets(ctx, id)
		if err != nil {
			return err
		}
		if sec == nil {
			sec = &Secrets{ID: id}
		}
		if capi != nil {
			sec.CAPI = capi
		}
		if csi != nil {
			sec.CSI = csi
		}
		sec.UpdatedAt = time.Now().UTC()
		return s.secrets().Upsert(ctx, id, s.seal(*sec, s.enc.Encrypt))
	})
}

// Secrets reads and opens an installation's secrets (nil when none are stored).
func (s *Store) Secrets(ctx context.Context, id string) (*Secrets, error) {
	var sec Secrets
	found, err := s.secrets().Get(ctx, id, &sec)
	if err != nil || !found {
		return nil, err
	}
	open := s.seal(sec, s.enc.Decrypt)
	return &open, nil
}

// seal returns sec with every secret value passed through f (Encrypt or Decrypt); the pointers are copied, not shared.
func (s *Store) seal(sec Secrets, f func(string) string) Secrets {
	opt := func(v string) string {
		if v == "" {
			return ""
		}
		return f(v)
	}
	for _, t := range []**TokenCredential{&sec.CAPI, &sec.CSI} {
		if *t != nil {
			c := **t
			c.Secret = opt(c.Secret)
			*t = &c
		}
	}
	if sec.SeedKubeconfig != nil {
		k := *sec.SeedKubeconfig
		k.Token = opt(k.Token)
		sec.SeedKubeconfig = &k
	}
	sec.K3sToken, sec.BootstrapToken = opt(sec.K3sToken), opt(sec.BootstrapToken)
	return sec
}

// SaveRun inserts or replaces a run.
func (s *Store) SaveRun(ctx context.Context, r *Run) error { return s.runs().Upsert(ctx, r.ID, r) }

// ActiveRun is the installation's run that is not terminal (nil when none).
func (s *Store) ActiveRun(ctx context.Context, installationID string) (*Run, error) {
	var r Run
	found, err := s.runs().FindOne(ctx, pgdoc.M{"installationId": installationID, "state": pgdoc.M{"$in": []string{RunQueued, RunRunning, RunWaiting}}}, &r)
	if err != nil || !found {
		return nil, err
	}
	return &r, nil
}
