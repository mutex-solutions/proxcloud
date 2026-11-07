package externalservice

// proxmox.go — typed accessors for the "proxmox" provider kind
// (config.provider == "proxmox"; dbaas/ceph-s3 precedent). Document shape:
//
//	{
//	  "name": "Proxmox Lab", "type": "CLOUD", "status": "PUBLIC",
//	  "config": {
//	    "provider": "proxmox",
//	    "region":   "pve",                     // 1 cluster = 1 region
//	    "regions":  {"pve": {"displayName": "Homelab"}},
//	    "services": {"compute": {"pve": true}, "network": {"pve": true},
//	                 "volumev3": {"pve": true}, "image": {"pve": true}},
//	    "apiUrl":      "https://pve1.lab:8006",
//	    "apiUrls":     ["https://pve2.lab:8006"], // failover: other nodes or a VIP
//	    "caCert":      "-----BEGIN CERTIFICATE-----…", // cluster CA pin; takes precedence over fingerprint
//	    "tokenId":     "proxcloud@pve!platform", // full API token identity (secret half below)
//	    "fingerprint": "AA:BB:…",              // SHA-256 cert pin; empty → system CA
//	    "insecureTls": false,
//	    "poolPrefix":  "pc-",                  // PoolFor derivation — the tenancy boundary
//	    "network": {                           // Phase 4
//	      "defaultZone": "overlay",
//	      "zones": {
//	        "overlay": {"kind": "evpn", "controller": "proxcloud", "asn": 65000,
//	                    "peers": ["10.0.0.1"], "exitNodes": ["pve1"], "mtu": 1450},
//	        "vlan":    {"kind": "vlan", "bridge": "vmbr0", "vlanMin": 100, "vlanMax": 199},
//	        "simple":  {"kind": "simple"}      // 1-node fallback; DHCP+SNAT need PVE ≥ 8.1
//	      },
//	      "publicNetworks": [{"id": "vmbr1", "name": "Internet",  // admin-designated inbound bridges, never
//	                          "cidr": "203.0.113.0/24",          // the management bridge; the IP pool
//	                          "gateway": "203.0.113.1",          // ProxCloud allocates public NICs from
//	                          "ranges": ["203.0.113.10-203.0.113.200"], // optional (default: whole cidr)
//	                          "exclude": ["203.0.113.53"]}],     // optional
//	      "cidrPool": "10.100.0.0/16",         // REQUIRED: every tenant CIDR lives inside it, never overlapping
//	      "isolation": "shared",               // "shared" (default zone + vnet DROP rules) | "evpn-per-project"
//	      "evpnController": "proxcloud"        // evpn-per-project: the controller every project zone uses
//	    },
//	    "storageProfiles": {                   // Phase 5 — honest capability flags
//	      "std": {"storages": {"*": "local-zfs"}, "shared": false, "snapshots": true,
//	              "thin": true, "liveMigrate": false, "replication": false}
//	    },                                     // validated on save: flags ≤ detected capabilities
//	    "capabilities": {                      // written by capability detection (services sync,
//	      "detectedAt": "2026-10-28T10:00:00Z",// admin re-detect, profile save) — never hand-edited
//	      "storages": {"local-zfs": {"type": "zfspool", "class": "performance", ...}}
//	    },
//	    "storageClasses": {                    // per storage: admin override + installer fio suggestion
//	      "nas-capacity": {"class": "capacity", "suggestedClass": "standard"}
//	    },
//	    "defaultStorageProfile": "std",
//	    "templates": {},                       // template catalog {slug: {vmid, node}}; a template on
//	                                           // shared storage clones to any node, a local one
//	                                           // only to its own node
//	    "storageHeadroomPct": 10,              // % of a storage kept free when placing disks
//	    "backupStorage": "nas-backup",         // shared store of server backups (NFS, CephFS, PBS); absent = none
//	    "importStorage": "nas-templates",      // store with import content for customer images; absent = no import
//	    "importUnknownSizeMaxGiB": 20,         // spool limit for a URL that states no size; absent = refuse such a URL
//	    "gpus": {},                            // Phase 6 — {mappingId: {"alias": "nvidia-4090"}}
//	    "overcommit": {"cpu": 4},              // scheduler capacity multiplier
//	    "metrics": {"source": "none"},         // until prometheus-pve-exporter is wired
//	    "provisioning": {"quota": {            // default project quota (PVEDefaultQuota), seeded at bootstrap
//	      "servers": 10, "vcpus": 20, "ramMb": 51200, "diskGb": 500,
//	      "networks": 5, "floatingIps": 2, "gpu": {"*": 0}}}
//	  },
//	  "secret": {"tokenSecret": "<uuid>"}      // encrypted at rest
//	}
//
// Tenancy model (ceph-s3 precedent): ONE admin token, the platform is the PDP. Every VM lives
// in the project's pool (PoolFor); every list filters by pool and every mutation re-verifies
// ownership against the platform's cached row — vnets, firewall groups and PCI mappings have no
// pool scoping in Proxmox, so the platform DB is the only tenancy boundary.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// IsProxmox reports whether this is a Proxmox VE provider (config.provider == "proxmox").
func (e *ExternalService) IsProxmox() bool { return e.Provider() == "proxmox" }

// PVERegion is config.region, falling back to the first configured region (ceph precedent).
func (e *ExternalService) PVERegion() string {
	if r := str(e.Config["region"]); r != "" {
		return r
	}
	if rs := e.RegionNames(); len(rs) > 0 {
		return rs[0]
	}
	return ""
}

// PVEPoolPrefix is the pool-prefix half of the PoolFor derivation (default "pc-").
func (e *ExternalService) PVEPoolPrefix() string {
	if p := str(e.Config["poolPrefix"]); p != "" {
		return p
	}
	return "pc-"
}

// PoolFor is the project's PVE pool id — the ONE place the derivation lives
// (RGWUIDFor precedent). Bootstrap persists the result on the service binding;
// teardown prefers the persisted value over re-deriving.
func (e *ExternalService) PoolFor(projectID string) string {
	return e.PVEPoolPrefix() + projectID
}

// PVEConfig assembles the pve.Config for pve.New from config + the decrypted secret.
func (e *ExternalService) PVEConfig() pve.Config {
	insecure, _ := e.Config["insecureTls"].(bool)
	return pve.Config{
		APIURL:         str(e.Config["apiUrl"]),
		APIURLs:        strList(e.Config["apiUrls"]),
		CACert:         str(e.Config["caCert"]),
		TokenID:        str(e.Config["tokenId"]),
		Secret:         str(e.secretMap()["tokenSecret"]),
		Fingerprint:    str(e.Config["fingerprint"]),
		InsecureTLS:    insecure,
		TimeoutSeconds: intOf(e.Config["timeoutSeconds"]),
	}
}

// PVEBackupStorage is config.backupStorage: the shared store server backups go to ("" = no backups offered).
func (e *ExternalService) PVEBackupStorage() string { return str(e.Config["backupStorage"]) }

// PVEImportStorage is config.importStorage: the store with import content the portal streams customer images to
// ("" = no image import offered).
func (e *ExternalService) PVEImportStorage() string { return str(e.Config["importStorage"]) }

// PVEImportUnknownSizeMaxGiB is config.importUnknownSizeMaxGiB: how large an image whose server states no size may get,
// spooled to the portal's temporary directory on the way. 0 (the default) refuses such a URL.
func (e *ExternalService) PVEImportUnknownSizeMaxGiB() int {
	if n, ok := e.Config["importUnknownSizeMaxGiB"].(float64); ok && n > 0 {
		return int(n)
	}
	return 0
}

// PVEOvercommitCPU is the scheduler's CPU overcommit factor (default 4).
func (e *ExternalService) PVEOvercommitCPU() float64 {
	oc, _ := e.Config["overcommit"].(map[string]any)
	if f, ok := oc["cpu"].(float64); ok && f > 0 {
		return f
	}
	return 4
}

// PVEStorageHeadroomPct is config.storageHeadroomPct: the % of a storage's capacity the
// scheduler and volume creates keep free (default 10; 0 disables the headroom, not the check).
func (e *ExternalService) PVEStorageHeadroomPct() float64 {
	if f, ok := e.Config["storageHeadroomPct"].(float64); ok && f >= 0 && f < 100 {
		return f
	}
	return 10
}

// PVETemplate is one admin-curated OS template (config.templates entry).
type PVETemplate struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	OSDistro  string `json:"osDistro,omitempty"`
	VMID      int    `json:"vmid"`
	Node      string `json:"node"`
	MinDiskGb int    `json:"minDiskGb,omitempty"`
	// Copies is a discovered template's per-node copy (node -> vmid). VMID/Node stay the primary.
	Copies map[string]int `json:"-"`
}

// OnNode returns the vmid to clone on node: the node's own copy, or 0 when it has none.
func (t PVETemplate) OnNode(node string) int {
	if id, ok := t.Copies[node]; ok {
		return id
	}
	if t.Node == node {
		return t.VMID
	}
	return 0
}

// PVEImageCatalog merges the templates discovered in the cluster with config.templates: an entry
// with vmid+node is an operator pin and wins; any other entry only overlays name, osDistro and
// minDiskGb on the discovered template of that slug. Sorted by slug.
func (e *ExternalService) PVEImageCatalog(found []pve.DiscoveredTemplate) []PVETemplate {
	pinned := map[string]bool{}
	out := e.PVETemplates()
	for _, t := range out {
		pinned[t.Slug] = true
	}
	overlay, _ := e.Config["templates"].(map[string]any)
	for _, d := range found {
		if pinned[d.Slug] {
			continue
		}
		distro, _, _ := strings.Cut(d.Slug, "-")
		t := PVETemplate{Slug: d.Slug, Name: d.Slug, OSDistro: distro, VMID: d.VMID, Node: d.Node, Copies: map[string]int{}}
		for node, c := range d.Copies {
			t.Copies[node] = c.VMID
			if node == d.Node {
				t.MinDiskGb = c.DiskGb
			}
		}
		if o, ok := overlay[d.Slug].(map[string]any); ok {
			if v := str(o["name"]); v != "" {
				t.Name = v
			}
			if v := str(o["osDistro"]); v != "" {
				t.OSDistro = v
			}
			if v := intOf(o["minDiskGb"]); v > 0 {
				t.MinDiskGb = v
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// PVETemplates reads the admin template catalog: config.templates =
// {"<slug>": {"vmid": 9000, "node": "pve1", "name": "Ubuntu 24.04", ...}}.
func (e *ExternalService) PVETemplates() []PVETemplate {
	m, _ := e.Config["templates"].(map[string]any)
	out := make([]PVETemplate, 0, len(m))
	for slug, raw := range m {
		t, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		tpl := PVETemplate{
			Slug:      slug,
			Name:      str(t["name"]),
			OSDistro:  str(t["osDistro"]),
			VMID:      intOf(t["vmid"]),
			Node:      str(t["node"]),
			MinDiskGb: intOf(t["minDiskGb"]),
		}
		if tpl.Name == "" {
			tpl.Name = slug
		}
		if tpl.VMID > 0 && tpl.Node != "" {
			out = append(out, tpl)
		}
	}
	return out
}

// PVENetwork is the admin network config (config.network).
type PVENetwork struct {
	DefaultZone    string
	Zones          map[string]pve.ZoneConfig
	CIDRPool       string
	BlockedCIDRs   []string // networks tenants must not reach, on top of the nodes' management networks
	PublicNetworks []PVEPublicNetwork
	// Isolation is pve.IsolationShared (default; anything unknown falls back to
	// it — still isolated) or pve.IsolationEVPN (a zone per project, templated
	// from DefaultZone, controller EVPNController).
	Isolation      string
	EVPNController string
}

// PVEMetadata is config.metadata: how servers get their NoCloud seed (pve/seed.go).
type PVEMetadata struct {
	// URL is the API's base URL as tenant vnets reach it ("" = the API's own base URL).
	URL string
	// ISOStorage holds seed CDs ("" = the first active iso storage on the VM's node, shared first).
	ISOStorage string
	// ForceISO seeds by CD even where a URL would work: tenants cannot reach the API at all.
	ForceISO bool
	// VendorData is the operator's vendor-data for every seeded server.
	VendorData string
}

// PVEMetadataSettings reads config.metadata.
func (e *ExternalService) PVEMetadataSettings() PVEMetadata {
	m, _ := e.Config["metadata"].(map[string]any)
	force, _ := m["forceIso"].(bool)
	return PVEMetadata{URL: str(m["url"]), ISOStorage: str(m["isoStorage"]), ForceISO: force, VendorData: str(m["vendorData"])}
}

// PVEPublicNetwork is one admin-designated inbound bridge/vnet plus its IP pool:
// CIDR (+ Gateway), optionally narrowed to Ranges ("a-b" or a single address),
// minus Exclude. No CIDR = public NICs cannot attach there.
type PVEPublicNetwork struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	CIDR    string   `json:"cidr,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	Ranges  []string `json:"ranges,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// PVENetworkConfig reads config.network into its typed shape.
func (e *ExternalService) PVENetworkConfig() PVENetwork {
	n, _ := e.Config["network"].(map[string]any)
	out := PVENetwork{
		DefaultZone:    str(n["defaultZone"]),
		CIDRPool:       str(n["cidrPool"]),
		BlockedCIDRs:   strList(n["blockedCidrs"]),
		Zones:          map[string]pve.ZoneConfig{},
		Isolation:      pve.IsolationShared,
		EVPNController: str(n["evpnController"]),
	}
	if str(n["isolation"]) == pve.IsolationEVPN {
		out.Isolation = pve.IsolationEVPN
	}
	if zones, ok := n["zones"].(map[string]any); ok {
		for key, raw := range zones {
			z, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			out.Zones[key] = pve.ZoneConfig{
				Key:        key,
				Kind:       str(z["kind"]),
				Bridge:     str(z["bridge"]),
				VlanMin:    intOf(z["vlanMin"]),
				VlanMax:    intOf(z["vlanMax"]),
				Controller: str(z["controller"]),
				ASN:        intOf(z["asn"]),
				Peers:      strList(z["peers"]),
				ExitNodes:  strList(z["exitNodes"]),
				VRFVxlan:   intOf(z["vrfVxlan"]),
				MTU:        intOf(z["mtu"]),
			}
		}
	}
	if pubs, ok := n["publicNetworks"].([]any); ok {
		for _, raw := range pubs {
			pn, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if id := str(pn["id"]); id != "" {
				name := str(pn["name"])
				if name == "" {
					name = id
				}
				out.PublicNetworks = append(out.PublicNetworks, PVEPublicNetwork{
					ID: id, Name: name, CIDR: str(pn["cidr"]), Gateway: str(pn["gateway"]),
					Ranges: strList(pn["ranges"]), Exclude: strList(pn["exclude"]),
				})
			}
		}
	}
	if out.DefaultZone == "" && len(out.Zones) == 1 {
		for k := range out.Zones {
			out.DefaultZone = k
		}
	}
	return out
}

// PVEStorageProfile is one admin-curated storage class (config.storageProfiles.<slug>):
// honest capability flags — the scheduler, FE badges, migration and billing all key
// off them; nothing silently falls back.
type PVEStorageProfile struct {
	Slug        string
	Label       string            // display name, e.g. "Capacity HDD (NFS)" ("" = the slug)
	Storages    map[string]string // node → storage id; "*" → same id on every node
	Shared      bool
	Snapshots   bool
	Thin        bool
	LiveMigrate bool
	Replication bool
	// Tier and Class are derived from the mapped storages (never configured on the profile):
	// Tier is full only when every storage is Ceph; Class is the lowest effective class of
	// its storages ("" while none was detected).
	Tier  string
	Class string
	// NoBootDisk / NoDBaaS are the profile's own bootDisk:false / dbaas:false downgrades.
	NoBootDisk bool
	NoDBaaS    bool
}

// Capacity reports whether the profile is HDD-class bulk storage (plan S18).
func (p PVEStorageProfile) Capacity() bool { return p.Class == pve.ClassCapacity }

// BootDisk reports whether a server's root disk may live on the profile: never on capacity class.
func (p PVEStorageProfile) BootDisk() bool { return !p.NoBootDisk && !p.Capacity() }

// DBaaS reports whether databases may use the profile: never on capacity class.
func (p PVEStorageProfile) DBaaS() bool { return !p.NoDBaaS && !p.Capacity() }

// DisplayName is the label, or the slug when none is set.
func (p PVEStorageProfile) DisplayName() string {
	if p.Label != "" {
		return p.Label
	}
	return p.Slug
}

// StorageFor resolves the profile's storage id on one node ("" = not available there).
func (p PVEStorageProfile) StorageFor(node string) string {
	if s, ok := p.Storages[node]; ok {
		return s
	}
	return p.Storages["*"]
}

// Placement returns the nodes the profile can place on right now — every node whose resolved
// storage (StorageFor) is currently active there, so "*" means the nodes where its storage is
// active, not every node — each with that storage's live state (the scheduler's free-space
// input). caps is pve.Client.DetectStorage's output.
func (p PVEStorageProfile) Placement(caps map[string]pve.StorageCapability) map[string]pve.StorageNodeState {
	out := map[string]pve.StorageNodeState{}
	for id, c := range caps {
		for node, st := range c.Nodes {
			if st.Active && p.StorageFor(node) == id {
				out[node] = st
			}
		}
	}
	return out
}

// classRank orders the storage classes; a profile's class is the lowest of its storages.
var classRank = map[string]int{pve.ClassCapacity: 1, pve.ClassStandard: 2, pve.ClassPerformance: 3}

// PVEStorageProfiles reads config.storageProfiles, deriving each profile's tier and class from
// config.capabilities and config.storageClasses.
func (e *ExternalService) PVEStorageProfiles() map[string]PVEStorageProfile {
	m, _ := e.Config["storageProfiles"].(map[string]any)
	out := map[string]PVEStorageProfile{}
	if len(m) == 0 {
		return out
	}
	detected := e.PVECapabilities().Storages
	overrides := e.PVEStorageClasses()
	classOf := func(id string) string {
		if c := overrides[id].Class; pve.ValidClass(c) {
			return c
		}
		return detected[id].Class
	}
	for slug, raw := range m {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		prof := PVEStorageProfile{Slug: slug, Label: str(p["label"]), Storages: map[string]string{}}
		if ss, ok := p["storages"].(map[string]any); ok {
			for node, id := range ss {
				if s := str(id); s != "" {
					prof.Storages[node] = s
				}
			}
		}
		prof.Shared, _ = p["shared"].(bool)
		prof.Snapshots, _ = p["snapshots"].(bool)
		prof.Thin, _ = p["thin"].(bool)
		prof.LiveMigrate, _ = p["liveMigrate"].(bool)
		prof.Replication, _ = p["replication"].(bool)
		full := len(prof.Storages) > 0
		for _, id := range prof.Storages {
			if c := classOf(id); c != "" && (prof.Class == "" || classRank[c] < classRank[prof.Class]) {
				prof.Class = c
			}
			if t := detected[id].Tier; t != pve.TierFull {
				full = false
			}
			// Downgrade-only, on every read: a claim the detected storage lacks is dropped, so a
			// profile written past ValidatePVEStorageProfiles (provider create, a storage that
			// lost a capability since) is never sold above what it can do.
			if len(detected) > 0 {
				c := detected[id]
				prof.Shared, prof.Snapshots, prof.Thin = prof.Shared && c.Shared, prof.Snapshots && c.Snapshots, prof.Thin && c.Thin
				prof.LiveMigrate, prof.Replication = prof.LiveMigrate && c.LiveMigrate, prof.Replication && c.Replication
			}
		}
		switch {
		case full:
			prof.Tier = pve.TierFull
		case len(detected) > 0:
			prof.Tier = pve.TierStandard
		}
		prof.NoBootDisk = p["bootDisk"] == false
		prof.NoDBaaS = p["dbaas"] == false
		// Snapshots on capacity class are not sold (NFS qcow2 internal snapshots block the VM).
		prof.Snapshots = prof.Snapshots && !prof.Capacity()
		if len(prof.Storages) > 0 {
			out[slug] = prof
		}
	}
	return out
}

// ValidatePVEStorageProfiles checks a config.storageProfiles value against the detected
// storage capabilities: every mapped storage exists, takes VM disks (content "images") and is
// active on its node ("*" = on at least one node), and a profile claims no capability flag its
// storages lack — admins may only downgrade. A capacity-class profile (its class comes from the
// storages' detected class or the storageClasses override) cannot claim snapshots, bootDisk or
// dbaas.
func ValidatePVEStorageProfiles(raw any, caps map[string]pve.StorageCapability, storageClasses any) error {
	if raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("storageProfiles must be an object")
	}
	profiles := (&ExternalService{Config: map[string]any{
		"storageProfiles": m, "capabilities": PVECapabilities{Storages: caps}, "storageClasses": storageClasses,
	}}).PVEStorageProfiles()
	for _, slug := range sortedKeys(m) {
		p, ok := profiles[slug]
		if !ok {
			return fmt.Errorf("storage profile %q maps no storage", slug)
		}
		// pm holds the raw claims: PVEStorageProfiles already clamps parsed flags to detection.
		pm, _ := m[slug].(map[string]any)
		if p.Capacity() {
			for _, f := range []string{"snapshots", "bootDisk", "dbaas"} {
				if pm[f] == true {
					return fmt.Errorf("storage profile %q is capacity class (HDD) and cannot claim %s", slug, f)
				}
			}
		}
		for _, node := range sortedKeys(p.Storages) {
			id := p.Storages[node]
			c, ok := caps[id]
			switch {
			case !ok:
				return fmt.Errorf("storage profile %q: storage %q does not exist", slug, id)
			case !c.HasContent("images"):
				return fmt.Errorf("storage profile %q: storage %q does not allow VM disks (content images)", slug, id)
			case node == "*" && !anyActive(c):
				return fmt.Errorf("storage profile %q: storage %q is not active on any node", slug, id)
			case node != "*" && !c.Nodes[node].Active:
				return fmt.Errorf("storage profile %q: storage %q is not active on node %s", slug, id, node)
			}
			for _, f := range []struct {
				name string
				has  bool
			}{
				{"shared", c.Shared}, {"snapshots", c.Snapshots}, {"thin", c.Thin},
				{"liveMigrate", c.LiveMigrate}, {"replication", c.Replication},
			} {
				if pm[f.name] == true && !f.has {
					return fmt.Errorf("storage profile %q claims %s but storage %q (%s) does not support it", slug, f.name, id, c.Type)
				}
			}
		}
	}
	return nil
}

func anyActive(c pve.StorageCapability) bool {
	for _, n := range c.Nodes {
		if n.Active {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PVECapabilities is config.capabilities: the last storage capability detection.
type PVECapabilities struct {
	DetectedAt time.Time                        `json:"detectedAt"`
	Storages   map[string]pve.StorageCapability `json:"storages"`
}

// PVECapabilities reads config.capabilities (zero value when never detected).
func (e *ExternalService) PVECapabilities() PVECapabilities {
	var out PVECapabilities
	if raw, ok := e.Config["capabilities"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &out)
		}
	}
	return out
}

// PVEStorageClass is config.storageClasses.<storage>: the admin override (Class, wins) and
// the installer's fio suggestion (SuggestedClass, display-only — never applied on its own).
type PVEStorageClass struct {
	Class          string `json:"class,omitempty"`
	SuggestedClass string `json:"suggestedClass,omitempty"`
}

// PVEStorageClasses reads config.storageClasses.
func (e *ExternalService) PVEStorageClasses() map[string]PVEStorageClass {
	m, _ := e.Config["storageClasses"].(map[string]any)
	out := map[string]PVEStorageClass{}
	for id, raw := range m {
		if c, ok := raw.(map[string]any); ok {
			out[id] = PVEStorageClass{Class: str(c["class"]), SuggestedClass: str(c["suggestedClass"])}
		}
	}
	return out
}

// PVEVolumeProfileFunc resolves a disk's storage profile and storage class for the volume sync
// (billing attributes "profile" and "storage_class"): the VM's own profile when it maps that
// storage on the node, else the first profile (by slug) that does; the class is the storage's
// effective class.
func (e *ExternalService) PVEVolumeProfileFunc() pve.VolumeProfileFunc {
	profiles := e.PVEStorageProfiles()
	slugs := sortedKeys(profiles)
	detected := e.PVECapabilities().Storages
	overrides := e.PVEStorageClasses()
	return func(node, storage, vmProfile string) (string, string) {
		class := detected[storage].Class
		if c := overrides[storage].Class; pve.ValidClass(c) {
			class = c
		}
		if profiles[vmProfile].StorageFor(node) == storage && storage != "" {
			return vmProfile, class
		}
		for _, slug := range slugs {
			if profiles[slug].StorageFor(node) == storage && storage != "" {
				return slug, class
			}
		}
		return "", class
	}
}

// PVEEffectiveStorageClass is the class a storage is treated as: a valid admin override,
// else the detected class ("" when the storage was never detected).
func (e *ExternalService) PVEEffectiveStorageClass(storage string) string {
	if c := e.PVEStorageClasses()[storage].Class; pve.ValidClass(c) {
		return c
	}
	return e.PVECapabilities().Storages[storage].Class
}

// DetectPVECapabilities runs storage detection against the cluster and persists the result
// under config.capabilities (a partial update, so a concurrent admin edit of other config
// keys is not clobbered).
func (s *Service) DetectPVECapabilities(ctx context.Context, es *ExternalService, c *pve.Client) (PVECapabilities, error) {
	caps, err := c.DetectStorage(ctx)
	if err != nil {
		return PVECapabilities{}, err
	}
	out := PVECapabilities{DetectedAt: time.Now().UTC(), Storages: caps}
	if _, err := s.repo.col.SetByID(ctx, es.ID, map[string]any{"config.capabilities": out}, nil); err != nil {
		return PVECapabilities{}, err
	}
	return out, nil
}

// PVEDefaultStorageProfile is config.defaultStorageProfile (falls back to the
// single configured profile).
func (e *ExternalService) PVEDefaultStorageProfile() string {
	if s := str(e.Config["defaultStorageProfile"]); s != "" {
		return s
	}
	profiles := e.PVEStorageProfiles()
	if len(profiles) == 1 {
		for k := range profiles {
			return k
		}
	}
	return ""
}

// PVEGPUMappingsFor resolves EVERY cluster PCI mapping carrying a normalized GPU
// alias (config.gpus = {"<mappingId>": {"alias": "nvidia-p40"}}). Several mappings
// may share one alias — the common "one mapping per card" operator shape — so this
// returns a set, sorted for deterministic scheduling. Empty = the alias is unmapped.
func (e *ExternalService) PVEGPUMappingsFor(alias string) []string {
	m, _ := e.Config["gpus"].(map[string]any)
	var out []string
	for mappingID, raw := range m {
		g, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if str(g["alias"]) == alias {
			out = append(out, mappingID)
		}
	}
	sort.Strings(out)
	return out
}

// PVEGPUAliasOf names the GPU model of a PCI mapping ("" = the mapping is not a GPU).
func (e *ExternalService) PVEGPUAliasOf(mappingID string) string {
	m, _ := e.Config["gpus"].(map[string]any)
	g, _ := m[mappingID].(map[string]any)
	return str(g["alias"])
}

// PVEGPUAliases lists the distinct GPU aliases the provider offers, each with its
// mapping ids (the capacity reads walk this).
func (e *ExternalService) PVEGPUAliases() map[string][]string {
	m, _ := e.Config["gpus"].(map[string]any)
	out := map[string][]string{}
	for mappingID, raw := range m {
		g, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if alias := str(g["alias"]); alias != "" {
			out[alias] = append(out[alias], mappingID)
		}
	}
	for a := range out {
		sort.Strings(out[a])
	}
	return out
}

// PVEQuotaKeys are the resource quota keys the Proxmox create paths enforce (Project.Quota,
// plus the "gpu" {model|"*": n} map). Absent key = unlimited.
var PVEQuotaKeys = []string{"servers", "vcpus", "ramMb", "diskGb", "networks", "floatingIps"}

// pveQuotaAliases maps the legacy alias keys (instances/cores/ram/gigabytes/floatingips) the
// generic provider quota editor (and the set_provider_default_quota MCP tool) still write onto
// the Proxmox keys. ram is MB in both.
var pveQuotaAliases = map[string]string{
	"instances": "servers", "cores": "vcpus", "ram": "ramMb", "gigabytes": "diskGb", "floatingips": "floatingIps",
}

// PVEDefaultQuota is the provider's default project quota (config.provisioning.quota) in
// Project.Quota keys, or nil when none is set. A Proxmox key wins over its legacy alias;
// negative values (e.g. -1) mean unlimited and are dropped, as are non-numbers and keys
// Proxmox does not enforce (volumes, ports, …). "gpu" is copied as-is.
func (e *ExternalService) PVEDefaultQuota() map[string]any {
	prov, _ := e.Config["provisioning"].(map[string]any)
	q, _ := prov["quota"].(map[string]any)
	out := map[string]any{}
	for alias, key := range pveQuotaAliases {
		if n, ok := intFrom(q[alias]); ok && n >= 0 {
			out[key] = n
		}
	}
	for _, key := range PVEQuotaKeys {
		if n, ok := intFrom(q[key]); ok && n >= 0 {
			out[key] = n
		}
	}
	if gpu, ok := q["gpu"].(map[string]any); ok && len(gpu) > 0 {
		out["gpu"] = gpu
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
