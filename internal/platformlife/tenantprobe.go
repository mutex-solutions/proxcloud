package platformlife

// tenantprobe.go — "Test the tenant path" (POST …/tenant-path-probe): the one check that opens the path the
// tenant-path row only reads. On demand, never part of a Check, because it costs a clone and two SDN applies:
//
//  1. From the ProxCloud host, find what listens in the load balancer range (a tenant cluster's API on 6443, a managed
//     database on 5432). Nothing listening, no probe: there is no target, and nothing is created.
//  2. Make a throwaway network in the provider's default tenant zone, with SNAT, as a tenant's network with outbound
//     internet is; clone a throwaway VM on the chosen node and put it there. The image is one of this portal's own, as
//     a tenant's server create resolves it (probeImage), cloned to the store a tenant's server disk goes to, and the
//     VM gets a fixed address over cloud-init, in every zone.
//  3. Through the guest agent, open one TCP connection from that VM to each listener, and one to the platform's own
//     API address, which tenants must NOT reach (the isolation rules).
//  4. Destroy the VM and the network, on every path, and at the start of the next probe whatever a crash left.
//
// The result depends on the node: the path breaks where the node that translates the tenant's traffic also hosts the
// platform machine that owns the address (docs/managed-services.md, "The path from tenants to the platform"). One
// probe tests one node. Nothing here was run on a lab.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// Probe states.
const (
	ProbeRunning = "running"
	ProbePassed  = "passed"
	ProbeFailed  = "failed" // the path is closed, or tenants reach the platform's API address
	ProbeError   = "error"  // the probe itself could not be done: nothing is known about the path

	probeDeadline = 8 * time.Minute // the test, from the first write to the verdict
	probeCleanup  = 4 * time.Minute // the cleanup, on its own clock: it also runs after the deadline
	probeHosts    = 32              // addresses of the load balancer range the ProxCloud host looks at
	probeNetName  = "tenant-path-probe"
	probeMaxOpen  = 8 // listeners the VM connects to
	probeTag      = "pc-probe"
)

// probePorts are what tenants reach in the load balancer range: a tenant cluster's API and a managed database.
var probePorts = []int{6443, 5432}

// TenantProbe is the last tenant path test of an installation (collection platformTenantProbe, id = the
// installation's). It holds no secret.
type TenantProbe struct {
	ID          string        `json:"id"`
	State       string        `json:"state"`
	Node        string        `json:"node"`
	Image       string        `json:"image,omitempty"`    // the catalog image the VM is cloned from (its slug)
	Template    int           `json:"template,omitempty"` // that image's template on Node
	Storage     string        `json:"storage,omitempty"`  // the store the clone's disk goes to; absent: the template's own
	RequestedBy string        `json:"requestedBy,omitempty"`
	StartedAt   time.Time     `json:"startedAt"`
	FinishedAt  *time.Time    `json:"finishedAt,omitempty"`
	Step        string        `json:"step,omitempty"` // what a running probe is doing
	VMID        int           `json:"vmid,omitempty"`
	Vnet        string        `json:"vnet,omitempty"`
	Source      string        `json:"source,omitempty"` // the VM's address on the probe network
	Targets     []ProbeTarget `json:"targets,omitempty"`
	Message     string        `json:"message"`
	// Left is what the cleanup could not remove, with what to do by hand. Empty when nothing is left.
	Left []string `json:"left,omitempty"`
	// Notes is what Proxmox may keep by itself, which the cleanup can neither see nor remove: the DHCP mapping of the
	// test VM's NIC in a zone with a DHCP server.
	Notes []string `json:"notes,omitempty"`
}

// CutOff reports a probe recorded as running for longer than a probe can run: its process died (an API restart), and
// nothing will ever finish it.
func (p *TenantProbe) CutOff(now time.Time) bool {
	return p.State == ProbeRunning && now.Sub(p.StartedAt) >= probeDeadline+probeCleanup
}

// View is the probe as a reader must see it: a probe that was cut off is an error that names what it may have left,
// not a test that runs for ever. The stored document is not changed; the next test replaces it.
func (p *TenantProbe) View(now time.Time) *TenantProbe {
	if !p.CutOff(now) {
		return p
	}
	v := *p
	v.State, v.Step = ProbeError, ""
	v.Message = fmt.Sprintf("The test started at %s did not end: the API was restarted while it ran, so nothing is known about the path. Test again: the next test first removes what this one left.", p.StartedAt.Format(time.RFC3339))
	v.Left = []string{fmt.Sprintf("Possibly a VM named %s with the tag %s in pool %s, and an SDN vnet with the alias %s. The next test removes both; to remove them by hand, destroy the VM, delete the vnet under Datacenter, SDN, VNets and apply.",
		ProbeVMName(p.ID), probeTag, pve.TemplatePool, pve.OwnershipAlias(probeProject(p.ID), probeNetName))}
	return &v
}

// ProbeTarget is one address the VM connects to. Expect open: the ProxCloud host opened it, so a tenant must too.
// Expect closed: the platform's API address, which the isolation rules keep from tenants. Result: open, refused (the
// host answered with a reset: the path is open) or none (no answer in 5 seconds).
type ProbeTarget struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	Expect  string `json:"expect"`
	Result  string `json:"result,omitempty"`
}

// ProbeRefused is a probe that was not started; nothing was created. Code is part of the API (docs/api.md).
type ProbeRefused struct{ Code, Message string }

func (e *ProbeRefused) Error() string { return e.Message }

func refuse(code, format string, a ...any) error {
	return &ProbeRefused{Code: code, Message: fmt.Sprintf(format, a...)}
}

// probeProject owns the probe's network in Proxmox, as a project owns a tenant's; no project has this id.
func probeProject(id string) string { return "pc-probe-" + id }

// ProbeVMName is the throwaway VM of an installation's probe, in pool pc-templates.
func ProbeVMName(id string) string { return "pc-probe-" + id }

// TenantProber runs the probe of one installation with the portal token.
type TenantProber struct {
	Portal *pve.Service
	Net    externalservice.PVENetwork
	// SDNLock is the site's SDN apply lock (Runner.SDNLock); nil in tests.
	SDNLock func(ctx context.Context) (release func(), acquired bool, err error)
	// Load and Save read and write the installation's probe document (Store.Probe, Store.SaveProbe).
	Load func(ctx context.Context, id string) (*TenantProbe, error)
	Save func(ctx context.Context, p *TenantProbe) error
	// Dial opens one TCP connection from the ProxCloud host; a net.Dialer with a 2 second timeout when nil.
	Dial func(ctx context.Context, address string) error
	Poll time.Duration // between two looks at the guest agent; 3 seconds when zero
	// Catalog is the provider's image catalog over what the cluster has (ExternalService.PVEImageCatalog): the list a
	// tenant's server create resolves its image in. The discovered templates alone when nil.
	Catalog func(found []pve.DiscoveredTemplate) []externalservice.PVETemplate
	// Imported reports a template that Template updates imported for this provider (it has the receipt). nil: none.
	Imported func(node string, vmid int) bool
	// DiskStore is the store a tenant server's disk goes to on a node: the provider's default storage profile there.
	// "" or nil: the template's own store, as a server create without a profile.
	DiskStore func(node string) string
	// Deadline is the test's limit, from the first write to the verdict; probeDeadline when zero (tests shorten it).
	Deadline time.Duration
}

func (x *TenantProber) defaults() {
	if x.Dial == nil {
		x.Dial = func(ctx context.Context, address string) error {
			c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", address)
			if err == nil {
				_ = c.Close()
			}
			return err
		}
	}
	if x.Poll <= 0 {
		x.Poll = 3 * time.Second
	}
	if x.Deadline <= 0 {
		x.Deadline = probeDeadline
	}
	if x.Catalog == nil {
		x.Catalog = (&externalservice.ExternalService{}).PVEImageCatalog
	}
}

// PlatformNodes are the nodes the settings place the control plane and the node groups on, sorted, each once.
func (in *Installation) PlatformNodes() []string {
	var out []string
	for _, c := range in.Spec.Clusters {
		out = append(out, c.ControlPlane.Node)
		for _, g := range c.NodeGroups {
			out = append(out, g.Nodes...)
		}
	}
	return slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(out))), func(s string) bool { return s == "" })
}

// probeImage is the image the test VM is cloned from on node, and its template there ("" and 0 when there is none):
// an Ubuntu or Debian image of the provider's catalog, with a copy on node, that is this portal's own. Own is what
// Template updates imported for this provider, or what the provider's config.templates pins by VM id (an entry
// without per-node copies). A published template in the pool that is neither may be another party's on a shared
// Proxmox cluster, and is never cloned: others lists those, for the refusal. The probe needs bash, timeout and the
// guest agent, which those images carry. Ubuntu before Debian, then the newest version, as the catalog ranks copies.
func probeImage(catalog []externalservice.PVETemplate, rows []pve.ClusterResource, node string, imported func(string, int) bool) (slug string, vmid int, others []string) {
	version := map[int]string{}
	for _, r := range rows {
		if r.Node == node {
			version[r.VMID] = pve.TagValue(r.Tags, pve.TagTemplateVersion)
		}
	}
	rank := func(t externalservice.PVETemplate) int {
		return slices.IndexFunc([]string{"ubuntu", "debian"}, func(d string) bool {
			return strings.EqualFold(t.OSDistro, d) || strings.HasPrefix(t.Slug, d)
		})
	}
	var best *externalservice.PVETemplate
	for i, t := range catalog {
		id := t.OnNode(node)
		if id == 0 || rank(t) < 0 {
			continue
		}
		if pinned := len(t.Copies) == 0; !pinned && (imported == nil || !imported(node, id)) {
			others = append(others, fmt.Sprintf("%s (VM %d)", t.Slug, id))
			continue
		}
		if best == nil || rank(t) < rank(*best) || (rank(t) == rank(*best) && version[id] > version[best.OnNode(node)]) {
			best = &catalog[i]
		}
	}
	if best == nil {
		return "", 0, others
	}
	return best.Slug, best.OnNode(node), nil
}

// Start checks that a probe can run and has a target, records it as running and returns; Run does the work. Every
// refusal comes before the first write to Proxmox.
// ponytail: two starts in the same instant both pass the running test, and the second removes the first's VM as a
// leftover (one of them then ends in "error"); a row lock on the probe document if that ever matters.
func (x *TenantProber) Start(ctx context.Context, in *Installation, node, actor string) (*TenantProbe, error) {
	x.defaults()
	if !in.Installed() {
		return nil, refuse("not_installed", "The platform is not installed yet, so nothing listens on the platform network and the path cannot be tested. Install first; until then the Check row tenant-path reads the layout.")
	}
	zone, ok := x.Net.Zones[x.Net.DefaultZone]
	if !ok || x.Net.CIDRPool == "" {
		return nil, refuse("no_tenant_zone", "The provider names no default zone or no network pool (config.network.defaultZone, cidrPool), so there is no tenant network to test from.")
	}
	// The zone as Proxmox has it decides, as for the tenant-path row: a zone made by hand may be of another kind.
	kind := zone.Type()
	zones, err := x.Portal.C.ListSDNZones(ctx)
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(zones, func(z pve.SDNZone) bool { return z.Zone == zone.Key }); i >= 0 {
		kind = zones[i].Type
	}
	if kind == "vlan" {
		return nil, refuse("vlan_zone", "The provider's default zone %s is a VLAN zone: no Proxmox node translates tenant traffic there, your router does, so the rule that closes the path does not apply. A test network would also need its own VLAN and gateway on that router, which ProxCloud cannot make. Test from a tenant's server instead: curl -k -m 5 https://<a tenant cluster's API address>:6443/healthz", zone.Key)
	}
	nodes := in.PlatformNodes()
	if node == "" && len(in.Spec.Clusters) > 0 {
		node = in.Spec.Clusters[0].ControlPlane.Node
	}
	if !slices.Contains(nodes, node) {
		return nil, refuse("invalid_node", "Node %q hosts no platform machine by the settings: choose one of %s. The path breaks only on a node that also hosts the platform machine that owns the address.", node, strings.Join(nodes, ", "))
	}
	if last, err := x.Load(ctx, in.ID); err != nil {
		return nil, err
	} else if last != nil && last.State == ProbeRunning && !last.CutOff(time.Now()) {
		return nil, refuse("probe_running", "A test of the tenant path started at %s is still running on %s.", last.StartedAt.Format(time.RFC3339), last.Node)
	}
	rows, err := x.Portal.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	image, tmpl, others := probeImage(x.Catalog(pve.DiscoverTemplates(rows)), rows, node, x.Imported)
	if tmpl == 0 {
		msg := fmt.Sprintf("No Ubuntu or Debian image of this portal is published on %s, so there is nothing to clone the test VM from. The test clones as a tenant's server create does, and only from an image that Template updates imported for this provider or that the provider's config.templates names with its VM id.", node)
		if len(others) > 0 {
			msg += " Published on " + node + " but not this portal's, so not used: " + strings.Join(others, ", ") + "."
		}
		return nil, refuse("no_os_template", "%s Import and publish one on that node under Template updates (with the scheduler off nothing is published by itself), then test again.", msg)
	}
	var store string
	if x.DiskStore != nil {
		store = x.DiskStore(node)
	}
	targets := x.listeners(ctx, in)
	if !slices.ContainsFunc(targets, func(t ProbeTarget) bool { return t.Expect == "open" }) {
		c := in.Spec.Clusters[0]
		return nil, refuse("no_target", "From the ProxCloud host nothing answers in the load balancer range %s on port 6443 or 5432 (the first %d addresses were tried): no tenant cluster or managed database exists yet, or the host itself has no route to the range. The test needs something that listens: create a database or a cluster, then test again. Nothing was created.",
			c.MetalLBRange, probeHosts)
	}
	p := &TenantProbe{ID: in.ID, State: ProbeRunning, Node: node, Image: image, Template: tmpl, Storage: store, RequestedBy: actor, StartedAt: time.Now().UTC(), Step: "starting",
		Targets: targets, Message: fmt.Sprintf("The test is running on %s. Its VM is a clone of the image %s (template %d).", node, image, tmpl)}
	return p, x.Save(ctx, p)
}

// listeners dials the first addresses of the load balancer range from the ProxCloud host: what opens is a target a
// tenant must open too. The platform's API address is added as a target tenants must not reach.
func (x *TenantProber) listeners(ctx context.Context, in *Installation) []ProbeTarget {
	var out []ProbeTarget
	if len(in.Spec.Clusters) == 0 {
		return nil
	}
	c := in.Spec.Clusters[0]
	prefix, _ := netip.ParsePrefix(in.Spec.Network.Prefix)
	r, ok := parseRange("", c.MetalLBRange, prefix, func(string, ...any) {})
	if !ok {
		return nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	a := r.first
	for i := 0; i < probeHosts; i++ {
		for _, port := range probePorts {
			wg.Add(1)
			go func(addr string, port int) {
				defer wg.Done()
				if x.Dial(ctx, net.JoinHostPort(addr, strconv.Itoa(port))) == nil {
					mu.Lock()
					out = append(out, ProbeTarget{Address: addr, Port: port, Expect: "open"})
					mu.Unlock()
				}
			}(a.String(), port)
		}
		if a == r.last {
			break
		}
		a = a.Next()
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b ProbeTarget) int {
		return strings.Compare(a.Address+strconv.Itoa(a.Port), b.Address+strconv.Itoa(b.Port))
	})
	// A tenant cluster's API has no allow-list in front of it; a managed database may have one (allowedCidrs) that admits
	// the ProxCloud host and not the test network, and would then read as a closed path. So databases are targets only
	// when no cluster API answers.
	if slices.ContainsFunc(out, func(t ProbeTarget) bool { return t.Port == 6443 }) {
		out = slices.DeleteFunc(out, func(t ProbeTarget) bool { return t.Port != 6443 })
	}
	if len(out) > probeMaxOpen {
		out = out[:probeMaxOpen]
	}
	if len(out) > 0 {
		out = append(out, ProbeTarget{Address: c.Endpoint, Port: 6443, Expect: "closed"})
	}
	return out
}

// Run does a started probe to its end and records it. It never returns before the cleanup ran; call it on a context
// that outlives the request.
func (x *TenantProber) Run(ctx context.Context, p *TenantProbe) {
	x.defaults()
	work, cancel := context.WithTimeout(ctx, x.Deadline)
	err := x.guarded(work, p)
	cancel()
	clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeCleanup)
	defer cancel()
	p.Left = x.cleanup(clean, p.ID, p.VMID)
	now, step := time.Now().UTC(), p.Step
	p.FinishedAt, p.Step = &now, ""
	if err != nil {
		p.State, p.Message = ProbeError, "The test could not be done, so nothing is known about the path: "+clip(err.Error())
		if errors.Is(err, context.DeadlineExceeded) {
			p.Message = fmt.Sprintf("The test did not finish in %s (last step: %s), so nothing is known about the path: %s", x.Deadline, step, clip(err.Error()))
		}
	} else {
		p.State, p.Message = verdict(p)
		p.Message += fmt.Sprintf(" The test VM was a clone of the image %s (template %d) on %s.", p.Image, p.Template, p.Node)
	}
	if len(p.Left) > 0 {
		p.Message += " The cleanup left something behind: see below."
	}
	_ = x.Save(clean, p)
}

// guarded runs test and turns a panic into its error: Run is called on a goroutine of the API process, where a panic
// would end the process, and the cleanup must still run.
func (x *TenantProber) guarded(ctx context.Context, p *TenantProbe) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the test stopped on an internal error: %v", r)
		}
	}()
	return x.test(ctx, p)
}

// verdict reads the targets: every listener must open (or refuse) from the tenant network, and the platform's API
// address must not answer.
func verdict(p *TenantProbe) (state, msg string) {
	var closed, leaked, opened []string
	var closedAPI bool // a tenant cluster's API (6443) did not open: no allow-list is in front of it
	for _, t := range p.Targets {
		at := net.JoinHostPort(t.Address, strconv.Itoa(t.Port))
		switch {
		case t.Expect == "open" && t.Result == "none":
			closed = append(closed, at)
			closedAPI = closedAPI || t.Port != 5432
		case t.Expect == "open":
			opened = append(opened, at)
		case t.Result != "none":
			leaked = append(leaked, at)
		}
	}
	from := fmt.Sprintf("a tenant network on %s (address %s, translated by the node as a tenant's is)", p.Node, p.Source)
	switch {
	case len(closed) > 0:
		msg = fmt.Sprintf("The path is closed: from %s no connection opened to %s, which the ProxCloud host opens.", from, strings.Join(closed, ", "))
		if len(opened) > 0 {
			msg += " It did open " + strings.Join(opened, ", ") + ": the result depends on which node holds each address."
		}
		if !closedAPI {
			// Only databases did not answer, and a database drops what its allow-list does not admit.
			return ProbeFailed, msg + " Only port 5432 was tried and did not answer. A managed database with allowedCidrs that admit the ProxCloud host but not " + p.Source +
				" drops the connection in the same way, so this result alone does not prove the path closed: check the database's allowedCidrs, or test again when a tenant cluster exists (port 6443 has no allow-list). If the path is closed, tenant Kubernetes workers on this node never join and tenants cannot reach managed databases: " + tenantPathGuide + "."
		}
		return ProbeFailed, msg + " Tenant Kubernetes workers on this node never join and tenants cannot reach managed databases: " + tenantPathGuide + "."
	case len(leaked) > 0:
		return ProbeFailed, fmt.Sprintf("The path to the load balancer range is open (%s), but %s also reached the platform's own API address %s, which tenants must not: check the Check row isolation and run a provider sync.",
			strings.Join(opened, ", "), from, strings.Join(leaked, ", "))
	}
	return ProbePassed, fmt.Sprintf("The path is open: from %s a connection opened to %s, and the platform's own API address did not answer, as the isolation rules want. This holds for %s; a tenant on another node takes another path.",
		from, strings.Join(opened, ", "), p.Node)
}

func (x *TenantProber) step(ctx context.Context, p *TenantProbe, step string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.Step = step
	return x.Save(ctx, p)
}

func (x *TenantProber) locked(ctx context.Context, fn func() error) error {
	if x.SDNLock == nil {
		return fn()
	}
	release, ok, err := x.SDNLock(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("another network change is being applied on this site; try again in a moment")
	}
	defer release()
	return fn()
}

// test is steps 2 and 3. What it creates carries the installation's probe name, so cleanup finds it without p.
func (x *TenantProber) test(ctx context.Context, p *TenantProbe) error {
	c := x.Portal.C
	if err := x.step(ctx, p, "removing what an earlier test left"); err != nil {
		return err
	}
	if left := x.cleanup(ctx, p.ID, 0); len(left) > 0 {
		return fmt.Errorf("an earlier test left something that could not be removed: %s", strings.Join(left, "; "))
	}
	if err := x.step(ctx, p, "making the test network"); err != nil {
		return err
	}
	var netw *pve.NetworkResult
	var dhcp bool
	if err := x.locked(ctx, func() error {
		used, err := x.Portal.SubnetCIDRs(ctx)
		if err != nil {
			return err
		}
		cidr, err := probeCIDR(x.Net.CIDRPool, used)
		if err != nil {
			return err
		}
		zone := x.Net.Zones[x.Net.DefaultZone]
		if x.Net.Isolation == pve.IsolationEVPN {
			if zone, err = x.Portal.EnsureProjectZone(ctx, probeProject(p.ID), zone, x.Net.EVPNController); err != nil {
				return fmt.Errorf("the test zone: %w", err)
			}
		} else if err := x.Portal.EnsureZone(ctx, zone); err != nil {
			return fmt.Errorf("the zone %s: %w", zone.Key, err)
		}
		// As a tenant's network with outbound internet: a DHCP range where the zone has a DHCP server, and SNAT, the
		// translation the path depends on.
		if dhcp, err = x.Portal.ZoneDHCP(ctx, zone); err != nil {
			return err
		}
		if netw, err = x.Portal.CreateNetwork(ctx, pve.CreateNetworkReq{ProjectID: probeProject(p.ID), Name: probeNetName, Zone: zone, CIDR: cidr, DHCP: dhcp, SNAT: true}); err != nil {
			return fmt.Errorf("the test network: %w", err)
		}
		return x.Portal.ApplyAndWait(ctx)
	}); err != nil {
		return err
	}
	prefix, err := netip.ParsePrefix(netw.CIDR)
	gw, err2 := netip.ParseAddr(netw.Gateway)
	if err != nil || err2 != nil {
		return fmt.Errorf("the test network %s has no usable range", netw.CIDR)
	}
	// The VM gets the address after the gateway, over cloud-init, also where the zone has a DHCP server: the test is of
	// the path, and must not depend on a DHCP answer. On the lab round of 2026-11-19 the VM asked a zone's DHCP, got no
	// address in the four minutes that were left, and the test ended without a verdict.
	p.Vnet = netw.Vnet
	p.Source = gw.Next().String()
	ipconfig := fmt.Sprintf("ip=%s/%d,gw=%s", p.Source, prefix.Bits(), netw.Gateway)

	if err := x.step(ctx, p, "cloning the test VM"); err != nil {
		return err
	}
	vmid, err := c.NextID(ctx)
	if err != nil {
		return err
	}
	p.VMID = vmid
	if err := x.Save(ctx, p); err != nil {
		return err
	}
	wait := func(up pve.UPID, err error) error {
		if err != nil {
			return err
		}
		return c.WaitTask(ctx, up)
	}
	if err := wait(c.CloneVM(ctx, p.Node, p.Template, pve.CloneOpts{NewID: vmid, Name: ProbeVMName(p.ID), Pool: pve.TemplatePool, Storage: p.Storage, Full: true})); err != nil {
		return fmt.Errorf("clone of the image %s (template %d) on %s: %w", p.Image, p.Template, p.Node, err)
	}
	cfg, err := c.GetVMConfig(ctx, p.Node, vmid)
	if err != nil {
		return err
	}
	params := map[string]string{
		"tags": pve.TagManaged + ";" + probeTag, "description": "ProxCloud test of the tenant path. Removed when the test ends.",
		"agent": "1", "onboot": "0", "ciupgrade": "0", "net0": "virtio,bridge=" + netw.Vnet, "ipconfig0": ipconfig,
	}
	if !hasCloudInit(cfg) {
		store, _, _ := strings.Cut(cfg.Str("scsi0"), ":")
		params["ide2"] = store + ":cloudinit"
	}
	if err := wait(c.SetVMConfig(ctx, p.Node, vmid, params, "cipassword", "cicustom")); err != nil {
		return fmt.Errorf("configuration of VM %d: %w", vmid, err)
	}
	if dhcp {
		// Proxmox gives a NIC in a zone with a DHCP server an address of the range by itself and writes it for the
		// zone's dnsmasq. The lab round found the line still there after the VM was destroyed and the vnet deleted.
		zone := x.Net.Zones[x.Net.DefaultZone].Key
		p.Notes = []string{fmt.Sprintf("Proxmox gave the test VM's NIC an address of the zone's DHCP range (the test does not use it) and may keep that mapping on %s after the VM is gone: a line with an address in %s in /etc/dnsmasq.d/%s/ethers and in /var/lib/misc/dnsmasq.%s.leases. "+
			"ProxCloud cannot read or remove those files. They do no harm while no other network uses %s; to remove them, delete those lines as root on %s.", p.Node, netw.CIDR, zone, zone, netw.CIDR, p.Node)}
	}
	if err := wait(c.StartVM(ctx, p.Node, vmid)); err != nil {
		return fmt.Errorf("start of VM %d: %w", vmid, err)
	}
	if err := x.step(ctx, p, "waiting for the test VM's guest agent and address"); err != nil {
		return err
	}
	for {
		if c.AgentPing(ctx, p.Node, vmid) == nil {
			if _, out, _, err := c.GuestRun(ctx, p.Node, vmid, 20*time.Second, "ip", "-4", "-o", "addr", "show"); err == nil {
				if addressIn(out, prefix) == p.Source {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the guest agent of VM %d did not answer with the address %s: %w", vmid, p.Source, ctx.Err())
		case <-time.After(x.Poll):
		}
	}
	if err := x.step(ctx, p, "opening the connections"); err != nil {
		return err
	}
	args := []string{"bash", "-c", probeScript, "probe"}
	for _, t := range p.Targets {
		args = append(args, net.JoinHostPort(t.Address, strconv.Itoa(t.Port)))
	}
	_, out, stderr, err := c.GuestRun(ctx, p.Node, vmid, time.Minute, args...)
	if err != nil {
		return fmt.Errorf("the connect test in VM %d: %w", vmid, err)
	}
	results := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if at, res, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			results[at] = res
		}
	}
	for i, t := range p.Targets {
		res := results[net.JoinHostPort(t.Address, strconv.Itoa(t.Port))]
		if res != "open" && res != "refused" && res != "none" {
			return fmt.Errorf("the connect test in VM %d gave no result for %s:%d (%s)", vmid, t.Address, t.Port, clip(strings.TrimSpace(stderr)))
		}
		p.Targets[i].Result = res
	}
	return nil
}

// probeScript opens one TCP connection per argument (address:port), all at once, and prints "<address:port> <result>":
// open, refused (a reset came back, so the path is open) or none (no answer in 5 seconds, or no route).
const probeScript = `for t in "$@"; do ( r=$(timeout 5 bash -c "exec 3<>/dev/tcp/${t%:*}/${t##*:}" 2>&1); c=$?
case "$r" in *refused*) s=refused;; *) if [ $c = 0 ]; then s=open; else s=none; fi;; esac; echo "$t $s" ) & done; wait`

// addressIn is the first address of `ip -4 -o addr show` that is inside prefix ("" when none is).
func addressIn(out string, prefix netip.Prefix) string {
	f := strings.Fields(out)
	for i, w := range f {
		if w != "inet" || i+1 == len(f) {
			continue
		}
		if p, err := netip.ParsePrefix(f[i+1]); err == nil && prefix.Contains(p.Addr()) {
			return p.Addr().String()
		}
	}
	return ""
}

// hasCloudInit: the clone has a Proxmox cloud-init drive, which carries its address to the guest.
func hasCloudInit(cfg pve.VMConfig) bool {
	for _, v := range cfg {
		if s, ok := v.(string); ok && strings.Contains(s, "cloudinit") {
			return true
		}
	}
	return false
}

// probeCIDR is a free /28 of the tenant pool, taken from its end: tenants' networks are carved from its start.
func probeCIDR(pool string, used []string) (string, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(pool))
	if err != nil || !p.Addr().Is4() || p.Bits() > 28 {
		return "", fmt.Errorf("the provider's cidrPool %q is not an IPv4 network of /28 or larger", pool)
	}
	var taken []netip.Prefix
	for _, u := range used {
		if q, err := netip.ParsePrefix(strings.TrimSpace(u)); err == nil {
			taken = append(taken, q)
		}
	}
	// ponytail: walks down from the end, at most 4096 blocks; a pool whose last 65536 addresses are all in use has none.
	for n, i := u32(lastAddr(p))&^15, 0; i < 4096 && p.Contains(addrOf(n)); n, i = n-16, i+1 {
		c := netip.PrefixFrom(addrOf(n), 28)
		if !slices.ContainsFunc(taken, c.Overlaps) {
			return c.String(), nil
		}
		if n < 16 {
			break
		}
	}
	return "", fmt.Errorf("no free /28 at the end of the provider's cidrPool %s", pool)
}

// cleanup removes the installation's probe VM and probe network, whoever made them (this probe or one that crashed),
// and answers what it could not remove. The next probe runs it first, so nothing a crash left stays for long.
//
// A VM is the probe's when it has the probe's name, is in pool pc-templates, is no template and carries the tag
// pc-probe, or is the VM this run cloned (own: a clone has its template's tags until the first config write). A VM
// with the name and without the tag is not destroyed and is reported: it is a clone whose run died before it was
// tagged, or someone else's.
func (x *TenantProber) cleanup(ctx context.Context, id string, own int) (left []string) {
	c := x.Portal.C
	rows, err := c.ClusterResources(ctx, "vm")
	if err != nil {
		left = append(left, "The VMs could not be listed ("+clip(err.Error())+"): look for a VM named "+ProbeVMName(id)+" in pool "+pve.TemplatePool+" and destroy it.")
	}
	for _, r := range rows {
		if r.Name != ProbeVMName(id) || r.Pool != pve.TemplatePool || r.Template == 1 {
			continue
		}
		if r.VMID != own && !pve.HasTag(r.Tags, probeTag) {
			left = append(left, fmt.Sprintf("VM %d (%s) on %s has the test VM's name but not the tag %s, so it was not destroyed. If it is a leftover of a test: as root on %s: qm stop %d; qm destroy %d --purge",
				r.VMID, r.Name, r.Node, probeTag, r.Node, r.VMID, r.VMID))
			continue
		}
		if up, err := c.StopVM(ctx, r.Node, r.VMID); err == nil {
			_ = c.WaitTask(ctx, up)
		}
		up, err := c.DeleteVM(ctx, r.Node, r.VMID)
		if err == nil {
			err = c.WaitTask(ctx, up)
		}
		if err != nil {
			left = append(left, fmt.Sprintf("VM %d (%s) on %s: %v. As root on %s: qm stop %d; qm destroy %d --purge", r.VMID, r.Name, r.Node, clip(err.Error()), r.Node, r.VMID, r.VMID))
		}
	}
	vnets, err := c.ListSDNVnets(ctx)
	if err != nil {
		return append(left, "The SDN vnets could not be listed ("+clip(err.Error())+"): look for a vnet with the alias "+pve.OwnershipAlias(probeProject(id), probeNetName)+" and delete it.")
	}
	mine := slices.DeleteFunc(vnets, func(v pve.SDNVnet) bool { return v.Alias != pve.OwnershipAlias(probeProject(id), probeNetName) })
	if len(mine) == 0 {
		return left
	}
	if err := x.locked(ctx, func() error {
		for _, v := range mine {
			if err := x.Portal.DeleteNetwork(ctx, probeProject(id), v.Vnet); err != nil {
				return err
			}
		}
		if x.Net.Isolation == pve.IsolationEVPN {
			if _, err := x.Portal.DeleteProjectZoneIfEmpty(ctx, probeProject(id)); err != nil {
				return err
			}
		}
		return x.Portal.ApplyAndWait(ctx)
	}); err != nil {
		left = append(left, fmt.Sprintf("The test network (vnet %s, alias %s): %v. Delete the vnet under Datacenter, SDN, VNets and apply.", mine[0].Vnet, mine[0].Alias, clip(err.Error())))
	}
	return left
}
