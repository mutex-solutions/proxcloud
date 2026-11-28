package platformlife

// sandbox_run.go — what a run does for spec.sandbox on the site (the rules and the renders are in sandbox.go).
//
// Stage cluster, before the platform cluster's Application names the pool (sandboxAccess): the pool pve.SandboxPool,
// the record of what is built, and what opens the pool: with shared one ACL row for the platform's own CAPMOX token,
// with separate a token of the sandbox cluster's own, minted as a tenant cluster's is. The portal token makes all of
// it with the rights it already holds for tenant pools; no root action. The shared group itself is built by the
// stage like every other group (Spec.platformCluster). At the stage's end (sandboxSettled) the group is recorded as
// built, or, when the settings switched sandboxes off, the row or token and the empty pool are deleted.
//
// Stage workload-platform: the proxy's certificate before the sandbox chart's wave (sandboxCertificate, shared), and
// after the last wave the separate cluster (sandboxCluster), which needs Kamaji and the tenants' Cluster API there,
// with the same certificate written inside it.
//
// What the portal needs to run sandboxes on them (the provider's sandbox network and pool in stage cluster, the two
// identities' kubeconfigs and the sandbox provider in stage register, their removal) is sandbox_provider.go.
//
// Remove deletes the separate cluster before the platform cluster, then what was written on Proxmox (remove.go).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"reflect"
	"slices"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

const (
	// CodeSandboxRefused: the run does not build what spec.sandbox asks for (a placement other than the built one, a
	// BOM without the sandbox chart, a certificate that is not stored). CodeSandboxAccessFailed: Proxmox refused the
	// pool, the ACL row or the token of the sandbox machines.
	CodeSandboxRefused      = "sandbox_refused"
	CodeSandboxAccessFailed = "sandbox_access_failed"

	resourcesFinalizer = "resources-finalizer.argocd.argoproj.io"
	// refreshAnnotation asks ArgoCD's controller for a comparison of the Application now ("normal": no new render).
	refreshAnnotation = "argocd.argoproj.io/refresh"
)

// sandboxAppName is the Application of the sandbox chart on the platform cluster (shared).
func (x *acq) sandboxAppName() string {
	if c := x.full.Feature(FeatureSandbox); c != nil {
		return ClusterName + "-" + c.Name
	}
	return ClusterName + "-" + FeatureSandbox
}

// platformVnet is the SDN zone and vnet of the platform network's bridge, "" and "" for a bridge of a node: a token
// that clones a VM onto a vnet needs SDN.Use on /sdn/zones/<zone>/<vnet>, which a mint grants only when it is named.
func (x *acq) platformVnet(ctx context.Context) (zone, vnet string, err error) {
	n := x.in.Spec.Network
	if n.Routed() {
		return RoutedZone, n.Bridge, nil
	}
	vnets, err := x.portal.ListSDNVnets(ctx)
	if err != nil {
		return "", "", err
	}
	for _, v := range vnets {
		if v.Vnet == n.Bridge {
			return v.Zone, v.Vnet, nil
		}
	}
	return "", "", nil
}

// sandboxAccess runs at the start of stage 7.
func (x *acq) sandboxAccess(ctx context.Context, seed *kamajik8s.Client) error {
	sp, built := x.in.Spec, x.in.Status.Sandbox
	want := sp.sandboxMode()
	switch {
	case want == "" && built == nil:
		return nil
	case want == "":
		if err := x.sandboxOffAllowed(ctx); err != nil {
			return err
		}
		return x.sandboxAppsGone(ctx, seed, built.Placement)
	case built != nil && built.Placement != want:
		return permanent(CodeSandboxRefused, "A run built sandboxes with placement %s and the settings say %s. Nothing was changed. Set spec.sandbox.placement off and apply, then set %s.", built.Placement, want, want)
	case x.full.Feature(FeatureSandbox) == nil:
		return permanent(CodeSandboxRefused, "The BOM of release %s has no sandbox chart, so this run builds no sandboxes. Nothing was changed: leave spec.sandbox out or set placement off.", x.full.Release)
	}
	if err := x.check(ctx); err != nil {
		return err
	}
	// Created only when the list does not name it: the stage passes here at every look, and Proxmox answers the create
	// of a pool that exists with a 500 in the node's log. A list that fails proves nothing: the create is sent.
	if pools, err := x.portal.ListPools(ctx); err != nil || !slices.ContainsFunc(pools, func(p pve.PoolInfo) bool { return p.PoolID == pve.SandboxPool }) {
		if err := x.portal.CreatePool(ctx, pve.SandboxPool, "ProxCloud sandbox machines (not tenant)"); err != nil {
			return classify(CodeSandboxAccessFailed, err)
		}
	}
	// The record first: a Remove or an off after a run that failed further on knows what to take away.
	sb := sp.Sandbox
	// The counts and nodes of this run, not the largest ever: after a smaller group, a larger one is machines nothing
	// has placed, and the capacity row must fail for them again (sandboxGrowth).
	next := SandboxStatus{Placement: want, NodeRange: sb.NodeRange, ProxyAddress: sb.ProxyAddress, Count: sb.Count, Nodes: slices.Clone(sb.Nodes)}
	if built != nil {
		next.ChartVersion, next.Certificate, next.BuiltAt = built.ChartVersion, built.Certificate, built.BuiltAt
	}
	if want == SandboxSeparate {
		if next.SystemCount, next.SystemNodes = sb.System.Count, slices.Clone(sb.System.Nodes); next.ChartVersion == "" {
			next.ChartVersion = x.full.TenantCharts.Kubernetes.Version
		}
	}
	if built == nil || !reflect.DeepEqual(*built, next) {
		if err := x.save(ctx, func(_ *Run, in *Installation, _ *Secrets) { in.Status.Sandbox = &next }); err != nil {
			return err
		}
	}
	// The provider's sandbox network and pool before any sandbox machine exists, and with them the isolation rules of
	// the range, written here and read back (sandboxIsolation): no machine is made behind rules that are not there,
	// and the installation's own Check does not fail until some provider sync. The fence pass itself starts once
	// stage register has stored the identity it reads the Nodes with (the sandbox chart makes it); until then the
	// sync writes no VM firewall and every machine keeps its unsecured taint, so no sandbox runs on one. On a later
	// apply the identity is stored already, and a new machine is fenced by the next sync after its clone.
	if err := x.sandboxSite(ctx, ""); err != nil {
		return err
	}
	if err := x.sandboxIsolation(ctx); err != nil {
		return err
	}
	if want == SandboxShared {
		// Looked at at every apply and written when it is missing, so a row deleted by hand comes back.
		return asAccess(x.portal.SetSandboxPoolAccess(ctx, false))
	}
	if x.sec.Sandbox != nil {
		return nil
	}
	scope := pve.ClusterTokenScope{Pool: pve.SandboxPool, TemplatePool: pve.TemplatePool, BootStorage: sp.Storage.Boot, ISOStorage: sp.Storage.ISO}
	var err error
	if scope.Zone, scope.Vnet, err = x.platformVnet(ctx); err != nil {
		return classify(CodeSandboxAccessFailed, err)
	}
	// A token an earlier attempt made and could not store has a secret nobody holds: it goes first, when there is one
	// (a delete of a token that does not exist is a 500 in the node's log).
	if err := x.portal.RevokeStaleClusterTokens(ctx, SandboxClusterName); err != nil {
		return classify(CodeSandboxAccessFailed, err)
	}
	t, err := x.portal.MintClusterTokens(ctx, SandboxClusterName, scope)
	if err != nil {
		return asAccess(err)
	}
	x.hide(t.CAPISecret)
	return x.save(ctx, func(_ *Run, _ *Installation, sec *Secrets) {
		sec.Sandbox = &TokenCredential{TokenID: t.CAPIID, Secret: t.CAPISecret}
	})
}

// asAccess is a refusal of Proxmox to open the sandbox pool, with the fix: the rights are the portal token's own.
func asAccess(err error) error {
	if err == nil {
		return nil
	}
	if ae, ok := err.(*pve.APIError); ok && (ae.Status == 403 || ae.Status == 400) {
		return permanent(CodeSandboxAccessFailed, "Proxmox refused the access of the sandbox machines to pool %s: %s. The portal token writes it with the rights the setup scripts gave it "+
			"(the platform role on /pool, the mint role on /storage, /sdn/zones and /nodes): the Check's row sandbox names what is missing. Run the managed-services script again as root on a node, then Retry.",
			pve.SandboxPool, clip(err.Error()))
	}
	return classify(CodeSandboxAccessFailed, err)
}

// sandboxAppsGone deletes, for settings that switched sandboxes off, what ArgoCD deployed for them, and waits until
// ArgoCD has taken it away: the sandbox chart's Application (shared; it is given the resources finalizer first, as the
// platform's Applications have none and a plain delete would leave every object) or the sandbox cluster's (separate:
// Cluster API deletes its machines before the Application goes). Needs the platform cluster's Kamaji and Cluster API
// alive for separate, which is why it comes before anything else of the stage.
//
// The access on Proxmox is taken away after this wait and not during it. Machines gone is not enough to take the
// separate cluster's token: CAPMOX is understood to open the Proxmox API with it at every look at the ProxmoxCluster
// and the ProxmoxMachines, their deletion included (read from its source, not tried on a lab), so a token revoked
// before their finalizers are off could leave them, the namespace and the Application in deletion for good. And the record that drives the rest of the off (the namespace,
// the provider's settings, the pool) is what removeSandboxAccess deletes last. So the wait itself is shortened
// instead: ArgoCD is asked to look again (refreshAnnotation).
func (x *acq) sandboxAppsGone(ctx context.Context, seed *kamajik8s.Client, placement string) error {
	name, ns := x.sandboxAppName(), sandboxNamespace
	if placement == SandboxSeparate {
		name, ns = SandboxClusterName, SandboxClusterName
	}
	app, err := seed.GetApplication(ctx, argoNamespace, name)
	if err != nil && !kamajik8s.NotFound(err) {
		return classify(CodeSeedAPIUnreachable, err)
	}
	if app != nil {
		if str(app, "metadata", "deletionTimestamp") == "" {
			if err := x.check(ctx); err != nil {
				return err
			}
			patch := map[string]any{"metadata": map[string]any{"finalizers": []any{resourcesFinalizer}}}
			if _, err := seed.MergePatchCR(ctx, argoGroup, argoVersion, "applications", argoNamespace, name, patch); err != nil {
				return classify(CodeSeedAPIUnreachable, err)
			}
			if err := seed.DeleteApplication(ctx, argoNamespace, name); err != nil && !kamajik8s.NotFound(err) {
				return classify(CodeSeedAPIUnreachable, err)
			}
		} else if str(app, "metadata", "annotations", refreshAnnotation) == "" {
			// ArgoCD is asked to look at the Application again, once per look of its own (it takes the annotation off
			// when it has): on the lab the Application of pc-sandbox stayed 4 min 40 s after the last object it deployed
			// was gone, until ArgoCD's next look by itself, and the token, its rows and the pool lived that long.
			if err := x.check(ctx); err != nil {
				return err
			}
			patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{refreshAnnotation: "normal"}}}
			if _, err := seed.MergePatchCR(ctx, argoGroup, argoVersion, "applications", argoNamespace, name, patch); err != nil && !kamajik8s.NotFound(err) {
				return classify(CodeSeedAPIUnreachable, err)
			}
		}
		return waiting(CodeMachineDeleteStuck, "Sandboxes are switched off: ArgoCD is deleting what Application %s deployed, and the run goes on when it is gone.", name)
	}
	wl, err := x.workload(ctx, seed)
	if err != nil || wl == nil {
		return err
	}
	// The namespace holds what the run wrote beside the chart: the proxy's certificate, the cluster's Proxmox token.
	if err := wl.DeleteNamespace(ctx, ns); err != nil && !kamajik8s.NotFound(err) {
		return classify(CodeWorkloadUnreachable, err)
	}
	if placement == SandboxShared {
		return x.sandboxCRDsGone(ctx, wl)
	}
	return nil
}

// sandboxSettled runs at the end of stage 7, when every machine of the platform cluster the settings name is Running
// and no other is left. Shared: the group stood once (BuiltAt). Off: the group's machines are gone, so the pool must
// be empty; then the ACL row or the token, the pool and the record go.
func (x *acq) sandboxSettled(ctx context.Context) error {
	built, want := x.in.Status.Sandbox, x.in.Spec.sandboxMode()
	switch {
	case built == nil:
		return nil
	case want == SandboxShared && built.BuiltAt == nil:
		now := time.Now().UTC()
		return x.save(ctx, func(_ *Run, in *Installation, _ *Secrets) { in.Status.Sandbox.BuiltAt = &now })
	case want != "":
		return nil
	}
	return x.removeSandboxAccess(ctx)
}

// removeSandboxAccess deletes what a run wrote on Proxmox for the sandbox machines, once the pool holds no VM: the ACL
// row (shared) or the token (separate), then the pool, then the record and the sealed secrets.
func (x *acq) removeSandboxAccess(ctx context.Context) error {
	built := x.in.Status.Sandbox
	if built == nil {
		return nil
	}
	rows, err := x.portal.ClusterResources(ctx, "vm")
	if err != nil {
		return classify(CodeSandboxAccessFailed, err)
	}
	if left := slices.DeleteFunc(rows, func(r pve.ClusterResource) bool { return r.Pool != pve.SandboxPool }); len(left) > 0 {
		return waiting(CodeMachineDeleteStuck, "%d VMs are still in pool %s (the first is VM %d on %s): the pool, and what opens it, are deleted when it is empty.", len(left), pve.SandboxPool, left[0].VMID, left[0].Node)
	}
	if err := x.check(ctx); err != nil {
		return err
	}
	// The provider stops naming the pool and the range first, so no sync fences on while the rest goes; then the
	// isolation rules follow the provider (the range's DROPs and ACCEPTs go, read back before anything else is
	// recorded: left to "the next provider sync" they failed the Check and stopped the next run); then the firewall
	// generations of the pool, which no pass would sweep any more.
	if x.r.SandboxSite != nil {
		if err := sandboxSiteErr(x.r.SandboxSite(ctx, x.in, &SandboxSite{Range: built.NodeRange})); err != nil {
			return err
		}
	}
	if err := x.sandboxIsolation(ctx); err != nil {
		return err
	}
	if err := (&pve.Service{C: x.portal}).RemoveSandboxFirewall(ctx, pve.SandboxPool); err != nil {
		return classify(CodeSandboxAccessFailed, err)
	}
	if built.Placement == SandboxShared {
		err = x.portal.SetSandboxPoolAccess(ctx, true)
	} else {
		err = x.portal.RevokeClusterTokens(ctx, SandboxClusterName)
	}
	if err == nil {
		err = x.portal.DeletePool(ctx, pve.SandboxPool)
	}
	if err != nil {
		return asAccess(err)
	}
	return x.save(ctx, func(_ *Run, in *Installation, sec *Secrets) {
		in.Status.Sandbox, sec.Sandbox, sec.SandboxTLSCert, sec.SandboxTLSKey = nil, nil, "", ""
	})
}

// sandboxCertificate writes, with placement shared, the Secret sandbox-proxy reads its wildcard certificate from,
// before the wave of the sandbox chart: the chart mounts it, and its Deployment never becomes available without
// (separate: sandboxCluster writes it inside pc-sandbox). The run makes a self-signed one (and a new one for another
// domain or 30 days before it ends); its key is sealed, the certificate is in the status for clients to trust. An
// operator's certificate (tls.mode secret) has no route that stores it yet: the save refuses it (SandboxProblems),
// and a record that names it all the same stops in applySandboxTLS.
func (x *acq) sandboxCertificate(ctx context.Context, wl *kamajik8s.Client) error {
	if x.in.Spec.sandboxMode() != SandboxShared {
		return nil
	}
	return x.applySandboxTLS(ctx, wl)
}

// applySandboxTLS makes or keeps the certificate and writes its Secret into the sandbox chart's namespace on target:
// the platform cluster (shared) or pc-sandbox (separate, sandboxCluster).
func (x *acq) applySandboxTLS(ctx context.Context, wl *kamajik8s.Client) error {
	sb := x.in.Spec.Sandbox
	if sb.TLS != nil && sb.TLS.Mode == TLSSecret {
		return permanent(CodeSandboxRefused, "spec.sandbox.tls.mode is secret and no certificate of the operator's is stored: this release has no route that stores one. Set tls.mode selfsigned, then apply again.")
	}
	cert, key := x.sec.SandboxTLSCert, x.sec.SandboxTLSKey
	if !certificateFor(cert, sb.Domain, time.Now().AddDate(0, 0, 30)) || key == "" {
		var err error
		if cert, key, err = selfSignedWildcard(sb.Domain, time.Now().UTC()); err != nil {
			return permanent(CodeSandboxRefused, "The certificate of sandbox-proxy could not be made: %s", clip(err.Error()))
		}
		x.hide(key)
		if err := x.save(ctx, func(_ *Run, in *Installation, sec *Secrets) {
			sec.SandboxTLSCert, sec.SandboxTLSKey = cert, key
			if in.Status.Sandbox != nil {
				in.Status.Sandbox.Certificate = cert
			}
		}); err != nil {
			return err
		}
	} else if err := x.check(ctx); err != nil {
		return err
	}
	if err := wl.EnsureNamespace(ctx, sandboxNamespace, map[string]string{"app.kubernetes.io/managed-by": ManagedBy}, nil); err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	s := object("v1", "Secret", sandboxNamespace, sandboxTLSSecret)
	s["type"], s["stringData"] = "kubernetes.io/tls", map[string]any{"tls.crt": cert, "tls.key": key}
	return classifyNil(CodeWorkloadUnreachable, wl.ApplyCore(ctx, "secrets", s))
}

func classifyNil(code string, err error) error {
	if err == nil {
		return nil
	}
	return classify(code, err)
}

// selfSignedWildcard is a certificate for domain and *.domain that is its own issuer (ECDSA P-256, five years): the
// one file a client of a lab adds to its trust store.
func selfSignedWildcard(domain string, now time.Time) (certPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "*." + domain}, DNSNames: []string{"*." + domain, domain},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(5, 0, 0),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})), nil
}

// certificateFor reports a PEM certificate that names *.domain and is still valid at until.
func certificateFor(certPEM, domain string, until time.Time) bool {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return false
	}
	c, err := x509.ParseCertificate(block.Bytes)
	return err == nil && slices.Contains(c.DNSNames, "*."+domain) && c.NotAfter.After(until)
}

// sandboxCluster builds, with placement separate, the cluster pc-sandbox once the platform cluster's waves are done:
// its namespace on the platform cluster with the kamaji group's placement, the Secret with its Proxmox token, and its
// Application on the seed's ArgoCD. Done when every machine of it is Running.
//
// ponytail: a machine CAPMOX gave up on is not told apart from one that is still being cloned; the stage ends at its
// deadline with the count. Read the ProxmoxMachines' conditions here, as stage 7 does, if that proves too blunt.
func (x *acq) sandboxCluster(ctx context.Context, seed, wl *kamajik8s.Client) error {
	if x.in.Spec.sandboxMode() != SandboxSeparate {
		return nil
	}
	if x.sec.Sandbox == nil {
		return permanent(CodeCredentialsMissing, "The Proxmox token of the sandbox cluster is not stored. Stage %s mints it: Retry.", stageCluster)
	}
	in := x.renderInput()
	app, err := SandboxApplication(x.full, in)
	if err != nil {
		return permanent(CodeComponentUnhealthy, "The sandbox cluster could not be rendered: %s", clip(err.Error()))
	}
	if err := x.check(ctx); err != nil {
		return err
	}
	if err := wl.EnsureNamespace(ctx, SandboxClusterName, map[string]string{"app.kubernetes.io/managed-by": ManagedBy}, groupPlacement("kamaji").Annotations()); err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	if err := wl.ApplyCore(ctx, "secrets", SandboxCredentials(x.full, in, PVEToken{ID: x.sec.Sandbox.TokenID, Secret: x.sec.Sandbox.Secret})); err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	if err := seed.ApplyApplication(ctx, app); err != nil {
		return classify(CodeComponentUnhealthy, err)
	}
	// The proxy's certificate inside pc-sandbox, as soon as its control plane answers: the sandbox chart there mounts
	// the Secret, and sandbox-proxy does not start without it.
	sc, err := x.sandboxClusterClient(ctx, wl)
	if err != nil {
		return err
	}
	if err := x.applySandboxTLS(ctx, sc); err != nil {
		return sandboxStarting(err)
	}
	machines, err := wl.ListCRs(ctx, capiGroup, capiVersion, "machines", SandboxClusterName, "")
	if err != nil {
		return classify(CodeWorkloadUnreachable, err)
	}
	total, running := 0, 0
	for _, g := range in.Sandbox.Groups {
		total += g.Count
	}
	for _, m := range machines {
		if str(m, "status", "phase") == "Running" {
			running++
		}
	}
	if running != total || len(machines) != total {
		// The machines are cloned, booted and joined on this stage's time, and what the stage had left when it came
		// here is a wave's 20 minutes (waveStall) less the last wave: 8.5 minutes went to two machines on the lab
		// (drill E4-C), and a slower store, a larger image or more machines take longer. They get the time stage
		// cluster gives the platform's own machines and the shared group's, once for a run, from the first look that
		// finds one missing.
		if x.receipt().SandboxMachinesAt == nil {
			now := time.Now().UTC()
			d := now.Add(sandboxMachinesStall)
			if err := x.save(ctx, func(run *Run, _ *Installation, _ *Secrets) {
				s := &run.Stages[x.si]
				if s.Receipt == nil {
					s.Receipt = &StageReceipt{}
				}
				s.Receipt.SandboxMachinesAt, s.LastProgressAt, s.Deadline = &now, &now, &d
			}); err != nil {
				return err
			}
		}
		return waiting(CodeControlPlaneTimeout, "The sandbox cluster %s: %d of %d machines are Running (%d exist).", SandboxClusterName, running, total, len(machines))
	}
	if st := x.in.Status.Sandbox; st != nil && st.BuiltAt == nil {
		// What is waited for next starts only now: Cilium on the new nodes, then the next try of the sandbox chart,
		// which was refused while Cilium's CRDs were missing. The add-on controller tries a failed release again with
		// growing gaps, and at the latest when its cache resyncs, every 10 minutes by its default, which the platform
		// does not change (CAAPH v0.6.4, main.go --sync-period; read, not measured). So that wait gets a wave's time,
		// counted from here and not from the machines' own deadline, once for a build: BuiltAt is set here and kept
		// until sandboxes are switched off.
		now := time.Now().UTC()
		d := now.Add(waveStall)
		if err := x.save(ctx, func(run *Run, in *Installation, _ *Secrets) {
			in.Status.Sandbox.BuiltAt = &now
			run.Stages[x.si].LastProgressAt, run.Stages[x.si].Deadline = &now, &d
		}); err != nil {
			return err
		}
	}
	// The sandbox chart is installed inside the cluster once its machines run; stage register reads the two
	// identities it makes. Waited for here, where the deadline is the long one.
	_, _, err = x.sandboxKubeconfigs(ctx, wl)
	return sandboxStarting(err)
}

// sandboxMachinesStall is how long stage workload-platform waits for the machines of the separate sandbox cluster to
// be Running, from its first look that finds one missing: the deadline of stage cluster, which builds the platform's
// own machines and the shared group's (stageDefs; TestSeparateSandboxWaitStartsAgainWhenItsMachinesRun holds the two
// equal). With the last wave before it and the sandbox chart after it, each waveStall at most, a first build of
// separate may keep the stage for 100 minutes before it fails; on the lab the machines took 8.5.
const sandboxMachinesStall = 60 * time.Minute

// sandboxStarting makes a failed call to the API of pc-sandbox a wait: its control plane was made minutes ago, and
// an API that does not answer yet is no reason to spend the stage's retries. The stage's deadline still ends it.
func sandboxStarting(err error) error {
	var se *stageErr
	if errors.As(err, &se) && (se.kind == kindRetry || se.kind == kindRefused) {
		return waiting(CodeControlPlaneTimeout, "The control plane of %s does not answer yet: %s", SandboxClusterName, se.msg)
	}
	return err
}

// removeSandboxCluster is the first deletion of a Remove on a platform with a separate sandbox cluster: its
// Application, with the cascade its finalizer carries, while the platform cluster still runs the Cluster API and the
// Kamaji that delete its machines and its control plane. Machines it leaves (a platform cluster that no longer
// answers) are deleted by their tag afterwards (deleteTaggedIn).
func (x *acq) removeSandboxCluster(ctx context.Context, seed *kamajik8s.Client) error {
	if st := x.in.Status.Sandbox; st == nil || st.Placement != SandboxSeparate {
		return nil
	}
	app, err := seed.GetApplication(ctx, argoNamespace, SandboxClusterName)
	if err != nil && !kamajik8s.NotFound(err) {
		return classify(CodeSeedAPIUnreachable, err)
	}
	if app == nil {
		return nil
	}
	if str(app, "metadata", "deletionTimestamp") == "" {
		if err := x.check(ctx); err != nil {
			return err
		}
		if err := seed.DeleteApplication(ctx, argoNamespace, SandboxClusterName); err != nil && !kamajik8s.NotFound(err) {
			return classify(CodeSeedAPIUnreachable, err)
		}
	}
	return waiting(CodeMachineDeleteStuck, "The sandbox cluster %s is deleted first: Cluster API and Kamaji on the platform cluster delete its machines and its control plane, and go after it.", SandboxClusterName)
}
