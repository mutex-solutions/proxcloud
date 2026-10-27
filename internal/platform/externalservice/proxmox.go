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
//	      "publicNetworks": [{"id": "vmbr0", "name": "Lab LAN"}], // admin-designated inbound bridges
//	      "cidrPool": "10.100.0.0/16"          // auto-carve /24 per tenant network
//	    },
//	    "storageProfiles": {                   // Phase 5 — honest capability flags
//	      "std": {"storages": {"*": "local-zfs"}, "shared": false, "snapshots": true,
//	              "thin": true, "liveMigrate": false, "replication": false}
//	    },
//	    "defaultStorageProfile": "std",
//	    "templates": {},                       // Phase 3/6 — template catalog {slug: {vmid|perNode}}
//	    "gpus": {},                            // Phase 6 — {mappingId: {"alias": "nvidia-4090"}}
//	    "overcommit": {"cpu": 4},              // scheduler capacity multiplier
//	    "metrics": {"source": "none"}          // until prometheus-pve-exporter is wired
//	  },
//	  "secret": {"tokenSecret": "<uuid>"}      // encrypted at rest
//	}
//
// Tenancy model (ceph-s3 precedent): ONE admin token, the platform is the PDP. Every VM lives
// in the project's pool (PoolFor); every list filters by pool and every mutation re-verifies
// ownership against the platform's cached row — vnets, firewall groups and PCI mappings have no
// pool scoping in Proxmox, so the platform DB is the only tenancy boundary.

import (
	"sort"

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
		TokenID:        str(e.Config["tokenId"]),
		Secret:         str(e.secretMap()["tokenSecret"]),
		Fingerprint:    str(e.Config["fingerprint"]),
		InsecureTLS:    insecure,
		TimeoutSeconds: intOf(e.Config["timeoutSeconds"]),
	}
}

// PVEOvercommitCPU is the scheduler's CPU overcommit factor (default 4).
func (e *ExternalService) PVEOvercommitCPU() float64 {
	oc, _ := e.Config["overcommit"].(map[string]any)
	if f, ok := oc["cpu"].(float64); ok && f > 0 {
		return f
	}
	return 4
}

// PVETemplate is one admin-curated OS template (config.templates entry).
type PVETemplate struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	OSDistro  string `json:"osDistro,omitempty"`
	VMID      int    `json:"vmid"`
	Node      string `json:"node"`
	MinDiskGb int    `json:"minDiskGb,omitempty"`
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
	PublicNetworks []PVEPublicNetwork
}

// PVEPublicNetwork is one admin-designated inbound bridge/vnet.
type PVEPublicNetwork struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PVENetworkConfig reads config.network into its typed shape.
func (e *ExternalService) PVENetworkConfig() PVENetwork {
	n, _ := e.Config["network"].(map[string]any)
	out := PVENetwork{
		DefaultZone: str(n["defaultZone"]),
		CIDRPool:    str(n["cidrPool"]),
		Zones:       map[string]pve.ZoneConfig{},
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
				out.PublicNetworks = append(out.PublicNetworks, PVEPublicNetwork{ID: id, Name: name})
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
	Storages    map[string]string // node → storage id; "*" → same id on every node
	Shared      bool
	Snapshots   bool
	Thin        bool
	LiveMigrate bool
	Replication bool
}

// StorageFor resolves the profile's storage id on one node ("" = not available there).
func (p PVEStorageProfile) StorageFor(node string) string {
	if s, ok := p.Storages[node]; ok {
		return s
	}
	return p.Storages["*"]
}

// Nodes returns the profile's node restriction (nil = all nodes, via "*").
func (p PVEStorageProfile) Nodes() map[string]bool {
	if _, ok := p.Storages["*"]; ok {
		return nil
	}
	out := map[string]bool{}
	for node := range p.Storages {
		out[node] = true
	}
	return out
}

// PVEStorageProfiles reads config.storageProfiles.
func (e *ExternalService) PVEStorageProfiles() map[string]PVEStorageProfile {
	m, _ := e.Config["storageProfiles"].(map[string]any)
	out := map[string]PVEStorageProfile{}
	for slug, raw := range m {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		prof := PVEStorageProfile{Slug: slug, Storages: map[string]string{}}
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
		if len(prof.Storages) > 0 {
			out[slug] = prof
		}
	}
	return out
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
