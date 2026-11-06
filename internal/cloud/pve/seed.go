package pve

// seed.go — the NoCloud seed of a server with user-data, vendor-data or instance metadata. PVE's own
// cloud-init drive cannot carry those: its only input is cicustom snippets, and snippets cannot be
// uploaded through the API. So the platform writes the seed itself, one of two ways:
//
//   - URL: smbios1 serial "ds=nocloud;s=<url>", the platform serves the files (per-VM token). Only
//     on a vnet with DHCP. cloud-init fetches a URL seed in its network stage, and until then the
//     only network config it has is the fallback (DHCP on the first NIC); a seed URL also makes the
//     local stage give up on any CD (lab-verified on cloud-init 25.1), so a static address cannot
//     come from anywhere else.
//   - ISO: a cidata CD in the VM's PVE cloud-init slot. Needs no network, so it carries the address
//     of a vnet without DHCP, and it is the fallback where the API is not reachable from tenants.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// SeedISOPrefix names a platform-written seed CD on storage.
const SeedISOPrefix = "pcseed-"

// SeedSpec is what a seed says about one server.
type SeedSpec struct {
	InstanceID string
	Hostname   string
	UserData   string
	// OperatorVendorData is the provider's own vendor-data (cloud-config or a script).
	OperatorVendorData string
	// PasswordHash is a crypt(3) hash for the image's default user.
	PasswordHash string
	SSHKeys      string // authorized_keys lines
	Metadata     map[string]string
	// Network is the rendered network-config ("" = none: DHCP on the first NIC).
	Network string
}

// Seed is the rendered NoCloud file set.
type Seed struct {
	MetaData      string
	UserData      string
	VendorData    string
	NetworkConfig string
}

// Files lists the non-empty parts in the shape BuildSeedISO takes.
func (s Seed) Files() map[string]string {
	out := map[string]string{"meta-data": s.MetaData, "user-data": s.UserData}
	if s.VendorData != "" {
		out["vendor-data"] = s.VendorData
	}
	if s.NetworkConfig != "" {
		out["network-config"] = s.NetworkConfig
	}
	return out
}

// RenderSeed renders the file set. JSON is valid YAML, which is what cloud-init reads meta-data,
// network-config and the body of a cloud-config as, and it leaves nothing for user input to break
// out of. The platform's own settings travel in vendor-data: on the same key a tenant's user-data
// wins, which is what a tenant setting their own password expects.
// Instance metadata sits under "meta" so a tenant key can never set instance-id, dsmode or seedfrom.
func RenderSeed(spec SeedSpec) (Seed, error) {
	meta := map[string]any{"instance-id": spec.InstanceID, "local-hostname": spec.Hostname}
	if len(spec.Metadata) > 0 {
		meta["meta"] = spec.Metadata
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return Seed{}, err
	}
	out := Seed{MetaData: string(raw), UserData: spec.UserData, NetworkConfig: spec.Network}
	if out.UserData == "" {
		// cloud-init requires the file; an empty cloud-config is the no-op.
		out.UserData = "#cloud-config\n{}\n"
	}
	// Later boots re-read the seed for its network: a public NIC attached after create needs it,
	// and a new instance-id would re-run the tenant's user-data.
	cc := map[string]any{"manage_etc_hosts": true, "updates": map[string]any{"network": map[string]any{"when": []string{"boot"}}}}
	if spec.PasswordHash != "" {
		cc["password"] = spec.PasswordHash
		cc["chpasswd"] = map[string]any{"expire": false}
		cc["ssh_pwauth"] = true
	}
	if keys := splitKeys(spec.SSHKeys); len(keys) > 0 {
		cc["ssh_authorized_keys"] = keys
	}
	raw, err = json.Marshal(cc)
	if err != nil {
		return Seed{}, err
	}
	platform := "#cloud-config\n" + string(raw) + "\n"
	if strings.TrimSpace(spec.OperatorVendorData) == "" {
		out.VendorData = platform
		return out, nil
	}
	out.VendorData, err = multipartVendorData(platform, spec.OperatorVendorData)
	return out, err
}

func splitKeys(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// multipartVendorData joins the platform's cloud-config and the operator's vendor-data in the one
// file cloud-init reads: a MIME multipart, which it processes for vendor-data like for user-data.
func multipartVendorData(platform, operator string) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	opType := "text/x-shellscript"
	if strings.HasPrefix(strings.TrimSpace(operator), "#cloud-config") {
		opType = "text/cloud-config"
	}
	for _, p := range []struct{ typ, name, content string }{
		{"text/cloud-config", "platform.cfg", platform},
		{opType, "operator", operator},
	} {
		part, err := w.CreatePart(textproto.MIMEHeader{
			"Content-Type":        {p.typ},
			"Content-Disposition": {`attachment; filename="` + p.name + `"`},
		})
		if err != nil {
			return "", err
		}
		if _, err := part.Write([]byte(p.content)); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return "Content-Type: multipart/mixed; boundary=\"" + w.Boundary() + "\"\nMIME-Version: 1.0\n\n" + body.String(), nil
}

// RenderNetworkConfig renders a version 1 network-config from a VM's netN + ipconfigN keys, the same
// shape PVE writes on its own drive. "" when every NIC is plain DHCP on net0 alone: that is
// cloud-init's fallback, and a URL seed answering 404 keeps it.
func RenderNetworkConfig(cfg VMConfig, dns []string) (string, error) {
	keys := nicKeys(cfg)
	slices.SortFunc(keys, func(a, b string) int {
		x, _ := strconv.Atoi(a[3:])
		y, _ := strconv.Atoi(b[3:])
		return x - y
	})
	var entries []any
	plain := true
	for _, key := range keys {
		idx := key[3:]
		ipcfg := cfg.Str("ipconfig" + idx)
		sub := map[string]any{"type": "dhcp4"}
		if addr, gw := ipconfigStatic(ipcfg); addr != "" {
			sub = map[string]any{"type": "static", "address": addr}
			if gw != "" {
				sub["gateway"] = gw
			}
			plain = false
		}
		if key != "net0" {
			plain = false
		}
		mac := nicMAC(cfg.Str(key))
		if mac == "" {
			return "", fmt.Errorf("pve: %s has no MAC address yet", key)
		}
		entries = append(entries, map[string]any{
			"type": "physical", "name": "eth" + idx, "mac_address": strings.ToLower(mac),
			"subnets": []any{sub},
		})
	}
	if plain {
		return "", nil
	}
	if len(dns) > 0 {
		entries = append(entries, map[string]any{"type": "nameserver", "address": dns})
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "config": entries})
	return string(raw), err
}

// ipconfigStatic returns the static CIDR and gateway of an ipconfigN value.
func ipconfigStatic(val string) (addr, gw string) {
	for _, p := range strings.Split(val, ",") {
		if v, ok := strings.CutPrefix(p, "ip="); ok && v != "dhcp" && v != "manual" {
			addr = v
		}
		if v, ok := strings.CutPrefix(p, "gw="); ok {
			gw = v
		}
	}
	return addr, gw
}

var nicMACRe = regexp.MustCompile(`(?i)\b(?:virtio|e1000|e1000e|rtl8139|vmxnet3)=([0-9a-f:]{17})\b`)

func nicMAC(val string) string {
	if m := nicMACRe.FindStringSubmatch(val); m != nil {
		return m[1]
	}
	return ""
}

// SeedSerial is the smbios1 serial of a URL seed.
func SeedSerial(seedURL string) string { return "ds=nocloud;s=" + seedURL }

// smbiosWithSerial sets the serial of an smbios1 value, keeping the rest (the uuid above all).
func smbiosWithSerial(val, serial string) string {
	v := smbiosWithoutSerial(val)
	if v != "" {
		v += ","
	}
	return v + "base64=1,serial=" + base64.StdEncoding.EncodeToString([]byte(serial))
}

func smbiosWithoutSerial(val string) string {
	var keep []string
	for _, p := range strings.Split(val, ",") {
		if p != "" && !strings.HasPrefix(p, "serial=") && !strings.HasPrefix(p, "base64=") {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, ",")
}

// cloudInitSlot finds the CD slot of a VM's cloud-init drive (PVE's own or a seed CD).
func cloudInitSlot(cfg VMConfig) string {
	for _, slot := range []string{"ide0", "ide1", "ide2", "ide3", "scsi1", "scsi30", "sata0"} {
		v := cfg.Str(slot)
		if strings.Contains(v, "cloudinit") || strings.Contains(v, "iso/"+SeedISOPrefix) {
			return slot
		}
	}
	return ""
}

// seedVolOf returns the volid of a VM's seed CD ("" = none).
func seedVolOf(cfg VMConfig) string {
	v := cfg.Str(cloudInitSlot(cfg))
	if !strings.Contains(v, "iso/"+SeedISOPrefix) {
		return ""
	}
	vol, _, _ := strings.Cut(v, ",")
	return vol
}

// PickSeedStorage is the storage a seed CD goes on for a VM on node: the configured one, else the
// first active iso storage there, shared ones first (a CD on node-local storage blocks migration).
func PickSeedStorage(caps map[string]StorageCapability, node, configured string) string {
	if configured != "" {
		return configured
	}
	var names []string
	for name := range caps {
		names = append(names, name)
	}
	slices.Sort(names)
	best := ""
	for _, name := range names {
		c := caps[name]
		if !c.HasContent("iso") || !c.Nodes[node].Active {
			continue
		}
		if c.Shared {
			return name
		}
		if best == "" {
			best = name
		}
	}
	return best
}

// AttachSeedISO uploads a seed CD to storage and puts it in the VM's cloud-init slot in place of the
// drive that was there (two cidata CDs would make the pick arbitrary). The VM must be stopped for the
// new CD to count at the next start; a CD already there is deleted. Returns the new volid.
func (s *Service) AttachSeedISO(ctx context.Context, node string, vmid int, storage string, seed Seed) (string, error) {
	iso, err := BuildSeedISO(seed.Files())
	if err != nil {
		return "", err
	}
	cfg, err := s.C.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s%d-%s.iso", SeedISOPrefix, vmid, randBase36(6))
	up, err := s.C.UploadISO(ctx, node, storage, name, iso)
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		if IsForbidden(err) {
			return "", conflictf("The provider's API token cannot write server seeds to storage %s (it needs Datastore.AllocateTemplate there)", storage)
		}
		return "", fmt.Errorf("pve seed: upload to %s: %w", storage, err)
	}
	vol := storage + ":iso/" + name
	old, slot := seedVolOf(cfg), cloudInitSlot(cfg)
	var del []string
	if slot == "" {
		slot = "ide2"
	} else if old == "" {
		// PVE's drive: removed first, or the same-key update would keep it allocated.
		del = []string{slot}
		cfgUp, derr := s.C.SetVMConfig(ctx, node, vmid, nil, del...)
		if derr == nil {
			derr = s.C.WaitTask(ctx, cfgUp)
		}
		if derr != nil {
			_ = s.C.DeleteStorageVolume(ctx, node, storage, "iso/"+name)
			return "", fmt.Errorf("pve seed: drop cloud-init drive: %w", derr)
		}
	}
	up, err = s.C.SetVMConfig(ctx, node, vmid, map[string]string{slot: vol + ",media=cdrom"})
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		_ = s.C.DeleteStorageVolume(ctx, node, storage, "iso/"+name)
		return "", fmt.Errorf("pve seed: attach: %w", err)
	}
	if old != "" {
		s.DeleteSeedVolume(ctx, node, old)
	}
	return vol, nil
}

// SetSeedURL points the VM's smbios1 serial at a URL seed and drops PVE's drive, whose user-data the
// URL seed replaces anyway.
func (s *Service) SetSeedURL(ctx context.Context, node string, vmid int, seedURL string) error {
	if u, err := url.Parse(seedURL); err != nil || u.Host == "" {
		return fmt.Errorf("pve seed: invalid url %q", seedURL)
	}
	cfg, err := s.C.GetVMConfig(ctx, node, vmid)
	if err != nil {
		return err
	}
	var del []string
	if slot := cloudInitSlot(cfg); slot != "" {
		del = []string{slot}
	}
	up, err := s.C.SetVMConfig(ctx, node, vmid, map[string]string{
		"smbios1": smbiosWithSerial(cfg.Str("smbios1"), SeedSerial(seedURL)),
	}, del...)
	if err != nil {
		return err
	}
	return s.C.WaitTask(ctx, up)
}

// DeleteSeedVolume removes a seed CD's file. Best-effort: at worst a few KiB stay on the storage.
func (s *Service) DeleteSeedVolume(ctx context.Context, node, volid string) {
	storage, vol, ok := strings.Cut(volid, ":")
	if !ok || !strings.Contains(vol, "iso/"+SeedISOPrefix) {
		return
	}
	_ = s.C.DeleteStorageVolume(ctx, node, storage, vol)
}

// NodeDNS lists the DNS servers the node resolves through: what PVE's own drive tells a guest.
func (c *Client) NodeDNS(ctx context.Context, node string) ([]string, error) {
	var d struct {
		DNS1 string `json:"dns1"`
		DNS2 string `json:"dns2"`
		DNS3 string `json:"dns3"`
	}
	if err := c.get(ctx, "/nodes/"+node+"/dns", nil, &d); err != nil {
		return nil, err
	}
	var out []string
	for _, v := range []string{d.DNS1, d.DNS2, d.DNS3} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// UploadISO stores a file on an iso-capable storage (POST .../upload, multipart). Needs
// Datastore.AllocateTemplate there. Returns the UPID of the copy.
func (c *Client) UploadISO(ctx context.Context, node, storage, filename string, data []byte) (UPID, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("content", "iso"); err != nil {
		return "", err
	}
	part, err := w.CreateFormFile("filename", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	var upid UPID
	path := "/nodes/" + node + "/storage/" + url.PathEscape(storage) + "/upload"
	err = c.doBody(ctx, "POST", path, &body, w.FormDataContentType(), &upid)
	return upid, err
}

// DeleteStorageVolume removes a volume from a storage (vol is the part after "storage:").
func (c *Client) DeleteStorageVolume(ctx context.Context, node, storage, vol string) error {
	return c.delete(ctx, "/nodes/"+node+"/storage/"+url.PathEscape(storage)+"/content/"+url.PathEscape(vol), nil, nil)
}

// ServerImageSlug reads the image slug out of a cached server row's data ("" = unknown).
func ServerImageSlug(data map[string]any) string {
	srv, _ := data["server"].(map[string]any)
	img, _ := srv["image"].(map[string]any)
	s, _ := img["id"].(string)
	return s
}
