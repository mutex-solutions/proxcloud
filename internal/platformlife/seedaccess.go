package platformlife

// seedaccess.go — what the routes do on a ready seed outside a run (design §3.5, §5): the live read of the
// components, the one-hour break-glass kubeconfig, and the rotation of the stored service-account credential.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// ErrNoSeed: the installation has no seed whose credential is stored.
var ErrNoSeed = errors.New("this installation has no ready seed")

// PhaseDegraded is derived on a live read and never stored: the installation is ready and a component is not.
const PhaseDegraded = "degraded"

// LiveState is what GET …?live=1 adds, read from the seed's ArgoCD at request time.
type LiveState struct {
	Phase      string          `json:"phase"`
	Components []LiveComponent `json:"components"`
	Error      string          `json:"error,omitempty"`
}

// LiveComponent is one Application's sync and health as ArgoCD reports them.
type LiveComponent struct {
	Name    string `json:"name"`
	Cluster string `json:"cluster"`
	Sync    string `json:"sync"`
	Health  string `json:"health"`
}

func storedSeed(in *Installation, sec *Secrets) (*kamajik8s.Client, error) {
	if in.Status.Seed == nil || sec == nil || sec.SeedKubeconfig == nil {
		return nil, ErrNoSeed
	}
	return seedClient(in.Status.Seed, sec.SeedKubeconfig.Token)
}

// Live reads the installation's Applications on the seed. A ready installation is degraded when the seed does not
// answer, lists no Application, or one is not Synced and Healthy.
func Live(ctx context.Context, in *Installation, sec *Secrets) LiveState {
	out := LiveState{Phase: in.Status.Phase, Components: []LiveComponent{}}
	c, err := storedSeed(in, sec)
	if err != nil {
		return out
	}
	apps, err := c.ListApplications(ctx, argoNamespace, "proxcloud.io/platform-installation="+in.ID)
	bad := len(apps) == 0
	if err != nil {
		out.Error = "The seed's API did not answer or refused the stored credential."
	}
	for _, a := range apps {
		lc := LiveComponent{Name: str(a, "metadata", "labels", "proxcloud.io/component"), Cluster: ClusterWorkload,
			Sync: str(a, "status", "sync", "status"), Health: str(a, "status", "health", "status")}
		if str(a, "spec", "destination", "server") == inCluster {
			lc.Cluster = ClusterSeed
		}
		bad = bad || lc.Sync != "Synced" || lc.Health != "Healthy"
		out.Components = append(out.Components, lc)
	}
	if bad && in.Status.Phase == PhaseReady {
		out.Phase = PhaseDegraded
	}
	return out
}

// BreakGlassKubeconfig asks the seed for a one-hour token of the portal's service account and wraps it in a kubeconfig
// for the operator. Nothing stores it; the caller audits who asked and when it expires, never the token.
func BreakGlassKubeconfig(ctx context.Context, in *Installation, sec *Secrets) (string, time.Time, error) {
	c, err := storedSeed(in, sec)
	if err != nil {
		return "", time.Time{}, err
	}
	token, expires, err := c.CreateServiceAccountToken(ctx, SeedPortalNamespace, SeedPortalAccount, time.Hour)
	if err != nil {
		return "", time.Time{}, err
	}
	kc, err := kamajik8s.BuildKubeconfig("pc-seed", in.Status.Seed.APIServer, []byte(in.Status.Seed.CACert), token)
	return kc, expires, err
}

// RotateSeedCredential replaces the stored seed credential: it creates the service account's next token Secret
// (proxcloud-portal-token-g<n+1>), checks that its token is cluster-admin, hands it to store, and only then deletes
// every older token Secret, which revokes their tokens. store must refuse while a run is unfinished.
func RotateSeedCredential(ctx context.Context, in *Installation, sec *Secrets, store func(ctx context.Context, old, next, token string) error) error {
	c, err := storedSeed(in, sec)
	if err != nil {
		return err
	}
	st := in.Status.Seed
	prefix := strings.TrimSuffix(SeedPortalTokenSecret, "1")
	n, err := strconv.Atoi(strings.TrimPrefix(st.TokenSecret, prefix))
	if err != nil {
		return fmt.Errorf("platform: the seed's token Secret %q is not %s<n>", st.TokenSecret, prefix)
	}
	next := prefix + strconv.Itoa(n+1)
	if err := c.ApplyCore(ctx, "secrets", with(object("v1", "Secret", SeedPortalNamespace, next),
		map[string]any{"type": "kubernetes.io/service-account-token"}, "kubernetes.io/service-account.name", SeedPortalAccount)); err != nil {
		return err
	}
	token := ""
	for range 20 { // the token controller fills the Secret
		data, err := c.GetSecretData(ctx, SeedPortalNamespace, next)
		if err != nil {
			return err
		}
		if token = string(data["token"]); token != "" {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if token == "" {
		return fmt.Errorf("platform: the seed did not fill %s/%s with a token", SeedPortalNamespace, next)
	}
	fresh, err := seedClient(st, token)
	if err != nil {
		return err
	}
	if ok, err := fresh.CanI(ctx, kamajik8s.AccessCheck{Verb: "*", Group: "*", Resource: "*"}); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("platform: the token of %s/%s is not cluster-admin; the stored credential was kept", SeedPortalNamespace, next)
	}
	if err := store(ctx, st.TokenSecret, next, token); err != nil {
		return err
	}
	// With the new credential, which proves the store holds one that works. Older Secrets a failed rotation left go too.
	all, err := fresh.ListSecrets(ctx, SeedPortalNamespace, "")
	if err != nil {
		return err
	}
	for _, s := range all {
		if name := str(s, "metadata", "name"); strings.HasPrefix(name, prefix) && name != next {
			if err := fresh.DeleteSecret(ctx, SeedPortalNamespace, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetSeedToken stores a rotated seed credential under the installation's run lock: ErrRunActive while a run is
// unfinished, ErrRevConflict when the seed's token Secret is no longer old (another rotation, or a rebuilt seed).
func (s *Store) SetSeedToken(ctx context.Context, id, old, next, token string) error {
	if !s.enc.HasKey() {
		return ErrNoEncryptionKey
	}
	return s.DB.WithTx(ctx, func(ctx context.Context) error {
		if err := s.DB.LockXact(ctx, "platform-run:"+id); err != nil {
			return err
		}
		var in Installation
		if found, err := s.installations().GetForUpdate(ctx, id, &in); err != nil {
			return err
		} else if !found {
			return ErrNotFound
		}
		if run, err := s.ActiveRun(ctx, id); err != nil {
			return err
		} else if run != nil {
			return ErrRunActive
		}
		sec, err := s.Secrets(ctx, id)
		if err != nil {
			return err
		}
		if in.Status.Seed == nil || in.Status.Seed.TokenSecret != old || sec == nil || sec.SeedKubeconfig == nil {
			return ErrRevConflict
		}
		in.Status.Seed.TokenSecret, in.UpdatedAt = next, time.Now().UTC()
		if _, err := s.installations().Replace(ctx, id, &in); err != nil {
			return err
		}
		sec.SeedKubeconfig.Token, sec.UpdatedAt = token, in.UpdatedAt
		return s.secrets().Upsert(ctx, id, s.seal(*sec, s.enc.Encrypt))
	})
}
