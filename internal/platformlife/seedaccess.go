package platformlife

// seedaccess.go — what the routes do on a ready seed outside a run (design §3.5, §5): the live read of the
// components, the one-hour break-glass kubeconfigs of the seed and of the platform cluster, and the rotation of the
// stored service-account credential.

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
	// Error says why the seed could not be read and what the operator does first; Code is then seed_api_unreachable,
	// the code a run fails with on the same seed. Both are derived like the phase and never stored.
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
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
// answer, lists no Application, or one is not Synced and Healthy. A seed that could not be read has a code and a
// message that tells a silent API (the VM is down, or runs without its Kubernetes) from one that refuses the credential.
func Live(ctx context.Context, in *Installation, sec *Secrets) LiveState {
	out := LiveState{Phase: in.Status.Phase, Components: []LiveComponent{}}
	c, err := storedSeed(in, sec)
	if err != nil {
		return out
	}
	apps, err := c.ListApplications(ctx, argoNamespace, "proxcloud.io/platform-installation="+in.ID)
	bad := len(apps) == 0
	if st := in.Status.Seed; err != nil && startingUp(err) {
		out.Code = CodeSeedAPIUnreachable
		out.Error = fmt.Sprintf("The seed's API at %s answers but refuses the stored credential. A seed that has just started does so for some seconds: load "+
			"this page again. If it stays, look on the console of seed VM %d on %s whether its Kubernetes keeps restarting.", st.APIServer, st.VMID, st.Node)
	} else if err != nil {
		out.Code = CodeSeedAPIUnreachable
		record := fmt.Sprintf("The record says %s.", in.Status.Phase)
		if in.Status.Phase == PhaseReady {
			record = "The record still says ready: nothing stores this state."
		}
		out.Error = fmt.Sprintf("The seed's API at %s does not answer. %s The platform cluster and "+
			"what runs on it keep running, but an apply or a Remove waits on the seed and then fails. Check that seed VM %d runs on %s and open its console "+
			"(qm terminal %d): the guide's section 'When the seed does not answer' has the steps.", st.APIServer, record, st.VMID, st.Node, st.VMID)
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

// ErrNoCluster: the seed holds no admin kubeconfig or CA of the platform cluster (stage 7 has not built it).
var ErrNoCluster = errors.New("this installation has no platform cluster yet")

// ErrWorkloadAPI wraps what the platform cluster's own API answered, to tell it from a failure of the seed.
var ErrWorkloadAPI = errors.New("the platform cluster's API failed")

// The platform cluster's break-glass identity: a service account that holds no token Secret, a ClusterRole that reads
// and a Role in ArgoCD's namespace. Written by PlatformKubeconfig, deleted with the cluster.
const (
	BreakGlassNamespace = "kube-system"
	BreakGlassAccount   = "proxcloud-break-glass"
	argoAdminSecret     = "argocd-initial-admin-secret" // written by argocd-server at its first start
)

// breakGlassRead is what the platform kubeconfig may get, list and watch on the whole cluster: what the guides'
// read steps look at (nodes, pods and their logs, events, Applications, tenant control planes, machines, databases,
// address pools, volumes). No Secret, and not bootstrap.cluster.x-k8s.io, whose objects carry join tokens.
var breakGlassRead = []any{
	map[string]any{"apiGroups": []any{""}, "verbs": []any{"get", "list", "watch"}, "resources": []any{"namespaces", "nodes", "pods", "pods/log",
		"services", "endpoints", "events", "configmaps", "persistentvolumes", "persistentvolumeclaims", "serviceaccounts", "resourcequotas"}},
	map[string]any{"verbs": []any{"get", "list", "watch"}, "resources": []any{"*"}, "apiGroups": []any{"apps", "batch", "networking.k8s.io",
		"discovery.k8s.io", "events.k8s.io", "storage.k8s.io", "apiextensions.k8s.io", "metrics.k8s.io", "argoproj.io", "kamaji.clastix.io",
		"cluster.x-k8s.io", "infrastructure.cluster.x-k8s.io", "controlplane.cluster.x-k8s.io", "ipam.cluster.x-k8s.io", "addons.cluster.x-k8s.io",
		"postgresql.cnpg.io", "metallb.io", "cilium.io", "cert-manager.io"}},
}

// breakGlassArgo is what it may do in ArgoCD's namespace on top: forward a port to a pod (kubectl port-forward of
// svc/argocd-server) and read the one Secret with the initial admin password. Both verbs on pods/portforward: the
// API server authorizes a WebSocket port-forward (an HTTP GET, kubectl's first try since 1.31) as get and a SPDY one
// (a POST) as create. The Service and its pods, which kubectl reads first, are in breakGlassRead.
var breakGlassArgo = []any{
	map[string]any{"apiGroups": []any{""}, "resources": []any{"pods/portforward"}, "verbs": []any{"get", "create"}},
	map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "resourceNames": []any{argoAdminSecret}, "verbs": []any{"get"}},
}

// PlatformKubeconfig is the break-glass kubeconfig of the platform cluster: a one-hour TokenRequest token of
// kube-system/proxcloud-break-glass, never the admin certificate Cluster API keeps on the seed. That certificate is
// only read into memory here, to write the account with its two roles (each call writes them again, so a changed
// rule set reaches an installed cluster without a run) and to ask for the token. A token the cluster grants for
// longer than the hour is not handed out. Nothing stores the token; the caller audits who asked and the expiry.
func PlatformKubeconfig(ctx context.Context, in *Installation, sec *Secrets) (string, time.Time, error) {
	seed, err := storedSeed(in, sec)
	if err != nil {
		return "", time.Time{}, err
	}
	admin, err := seed.GetSecretData(ctx, ClusterName, kubeconfigSecret)
	if err != nil {
		return "", time.Time{}, err
	}
	ca, err := seed.GetSecretData(ctx, ClusterName, ClusterName+"-ca")
	if err != nil {
		return "", time.Time{}, err
	}
	if len(admin["value"]) == 0 || len(ca["tls.crt"]) == 0 {
		return "", time.Time{}, ErrNoCluster
	}
	wl, err := kamajik8s.New(string(admin["value"]))
	if err != nil { // the parse error could quote the kubeconfig
		return "", time.Time{}, ErrNoCluster
	}
	managed := func(obj map[string]any) map[string]any {
		obj["metadata"].(map[string]any)["labels"] = map[string]any{"app.kubernetes.io/managed-by": ManagedBy}
		return obj
	}
	subjects := []any{map[string]any{"kind": "ServiceAccount", "name": BreakGlassAccount, "namespace": BreakGlassNamespace}}
	err = errors.Join(
		wl.ApplyCore(ctx, "serviceaccounts", managed(object("v1", "ServiceAccount", BreakGlassNamespace, BreakGlassAccount))),
		wl.ApplyClusterScoped(ctx, rbacGroup, "v1", "clusterroles", with(managed(object(rbacGroup+"/v1", "ClusterRole", "", BreakGlassAccount)),
			map[string]any{"rules": breakGlassRead})),
		wl.ApplyClusterScoped(ctx, rbacGroup, "v1", "clusterrolebindings", with(managed(object(rbacGroup+"/v1", "ClusterRoleBinding", "", BreakGlassAccount)),
			map[string]any{"roleRef": map[string]any{"apiGroup": rbacGroup, "kind": "ClusterRole", "name": BreakGlassAccount}, "subjects": subjects})),
		wl.ApplyCR(ctx, rbacGroup, "v1", "roles", with(managed(object(rbacGroup+"/v1", "Role", argoNamespace, BreakGlassAccount)),
			map[string]any{"rules": breakGlassArgo})),
		wl.ApplyCR(ctx, rbacGroup, "v1", "rolebindings", with(managed(object(rbacGroup+"/v1", "RoleBinding", argoNamespace, BreakGlassAccount)),
			map[string]any{"roleRef": map[string]any{"apiGroup": rbacGroup, "kind": "Role", "name": BreakGlassAccount}, "subjects": subjects})),
	)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", ErrWorkloadAPI, err)
	}
	token, expires, err := wl.CreateServiceAccountToken(ctx, BreakGlassNamespace, BreakGlassAccount, time.Hour)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", ErrWorkloadAPI, err)
	}
	if time.Until(expires) > time.Hour+time.Minute {
		return "", time.Time{}, fmt.Errorf("%w: it granted a token until %s, longer than the hour asked for", ErrWorkloadAPI, expires.UTC().Format(time.RFC3339))
	}
	kc, err := kamajik8s.BuildKubeconfig(ClusterName, wl.Server(), ca["tls.crt"], token)
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
	// A rotation that stops before the store holds the new token deletes the Secret it made: nothing stores its token,
	// and it would stay a second cluster-admin credential on the seed.
	drop := func(err error) error {
		_ = c.DeleteSecret(context.WithoutCancel(ctx), SeedPortalNamespace, next)
		return err
	}
	token := ""
	for range 20 { // the token controller fills the Secret
		data, err := c.GetSecretData(ctx, SeedPortalNamespace, next)
		if err != nil {
			return drop(err)
		}
		if token = string(data["token"]); token != "" {
			break
		}
		select {
		case <-ctx.Done():
			return drop(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if token == "" {
		return drop(fmt.Errorf("platform: the seed did not fill %s/%s with a token", SeedPortalNamespace, next))
	}
	fresh, err := seedClient(st, token)
	if err != nil {
		return drop(err)
	}
	if ok, err := fresh.CanI(ctx, kamajik8s.AccessCheck{Verb: "*", Group: "*", Resource: "*"}); err != nil {
		return drop(err)
	} else if !ok {
		return drop(fmt.Errorf("platform: the token of %s/%s is not cluster-admin; the stored credential was kept", SeedPortalNamespace, next))
	}
	if err := store(ctx, st.TokenSecret, next, token); err != nil {
		// Only a refusal is known to have stored nothing. After any other error the commit may have happened, and the
		// Secret may be the stored credential's: it stays, and the next run's stage 6 sweeps whichever is not.
		if errors.Is(err, ErrRunActive) || errors.Is(err, ErrRevConflict) || errors.Is(err, ErrNoEncryptionKey) || errors.Is(err, ErrNotFound) {
			return drop(err)
		}
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
