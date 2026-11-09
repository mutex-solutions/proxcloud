package project

// nocloud_pve.go — the NoCloud seed of a Proxmox server: user-data, vendor-data and instance
// metadata (pve/seed.go says why PVE's own drive cannot carry them). A server without any of those
// stays on PVE's drive.
//
// GET /api/v1/nocloud/{token}/{file} serves a URL seed without a session: the guest has no other
// credential. The token is per VM, only its hash is stored, and deleting the server revokes it.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const (
	seedModeURL = "url"
	seedModeISO = "iso"
	// maxUserData matches what clouds commonly accept.
	maxUserData = 64 << 10
	maxMetadata = 64
	// seedOrphanGrace is how long after a create the server's row may still be missing.
	seedOrphanGrace = 5 * time.Minute
)

// ServerSeed is one server's seed (collection pveServerSeed). _id = hash of the URL token, or for an
// ISO seed a random id nothing can be fetched with. UserData is encrypted: it often holds secrets.
type ServerSeed struct {
	ID            string            `json:"id"`
	ServiceID     string            `json:"serviceId"`
	ProjectID     string            `json:"projectId"`
	ServerID      string            `json:"serverId"`
	Node          string            `json:"node"`
	Mode          string            `json:"mode"`
	ISO           string            `json:"iso,omitempty"`
	InstanceID    string            `json:"instanceId"`
	Hostname      string            `json:"hostname"`
	UserData      string            `json:"userData,omitempty"`
	LoginUser     string            `json:"loginUser,omitempty"`
	PasswordHash  string            `json:"passwordHash,omitempty"`
	SSHKeys       string            `json:"sshKeys,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	NetworkConfig string            `json:"networkConfig,omitempty"`
	CreatedAt     *time.Time        `json:"createdAt,omitempty"`
	// Retired sources stay encrypted for cleanup/recovery, but can never seed a
	// future VM reusing this vmid. Explicit deletion may retire without global read.
	Retired bool `json:"retired,omitempty"`
	// The revision read by this request; not persisted. Rebuild changes iid, so
	// a NIC reseed holding an older source cannot overwrite the new generation.
	readInstanceID string
}

// SeedRepo persists the seeds.
type SeedRepo struct {
	col       *pgdoc.Store
	enc       *textcrypt.Encryptor
	artifacts *pve.SeedArtifactRepo
}

func NewSeedRepo(db *pgdoc.DB, enc *textcrypt.Encryptor) *SeedRepo {
	return &SeedRepo{col: db.C("pveServerSeed"), enc: enc, artifacts: pve.NewSeedArtifactRepo(db)}
}

// SetSeeds wires the server-seed store (nil = user-data is refused).
func (h *Handler) SetSeeds(r *SeedRepo) { h.seeds = r }

func (r *SeedRepo) put(ctx context.Context, s *ServerSeed) error {
	stored := *s
	if s.UserData != "" {
		stored.UserData = r.enc.Encrypt(s.UserData)
	}
	return r.col.Upsert(ctx, s.ID, &stored)
}

func (r *SeedRepo) decrypted(s *ServerSeed) *ServerSeed {
	if s.UserData != "" {
		s.UserData = r.enc.Decrypt(s.UserData)
	}
	return s
}

func (r *SeedRepo) byToken(ctx context.Context, token string) (*ServerSeed, error) {
	var s ServerSeed
	found, err := r.col.Get(ctx, hashSeedToken(token), &s)
	if err != nil || !found {
		return nil, err
	}
	return r.decrypted(&s), nil
}

// ForServer returns the project's seed for a server (nil = it runs on PVE's drive). PVE reuses vmids,
// so a seed left behind by another project's deleted VM can share the service and the vmid: its
// user-data must never reach this VM.
func (r *SeedRepo) ForServer(ctx context.Context, serviceID, projectID, serverID string) (*ServerSeed, error) {
	var s ServerSeed
	found, err := r.col.FindOne(ctx, pgdoc.M{"serviceId": serviceID, "projectId": projectID, "serverId": serverID, "retired": pgdoc.M{"$ne": true}}, &s)
	if err != nil || !found {
		return nil, err
	}
	s.readInstanceID = s.InstanceID
	return r.decrypted(&s), nil
}

func hashSeedToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var seedHostnameDrop = regexp.MustCompile(`[^a-z0-9-]+`)

// seedHostname makes a DNS label out of a server name.
func seedHostname(name string) string {
	h := strings.Trim(seedHostnameDrop.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(h) > 63 {
		h = strings.TrimRight(h[:63], "-")
	}
	if h == "" {
		return "server"
	}
	return h
}

// pveSeedInput is what a server create asks of its seed.
type pveSeedInput struct {
	Name, UserData, Password, LoginUser, SSHKeys string
	Metadata                                     map[string]string
}

// seedLoginUserRe is a name useradd takes on every image. It goes into a JSON document, so this is about a create that
// would otherwise boot without its login, not about quoting.
var seedLoginUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// pveSeedMetadata validates a create's instance metadata against the admin's options: an option a
// tenant may not edit is not theirs to set, and a value outside an option's list is refused.
func (h *Handler) pveSeedMetadata(ctx context.Context, serviceID, region string, data map[string]any) (map[string]string, *httpx.HTTPError) {
	raw, _ := data["metadata"].(map[string]any)
	out := map[string]string{}
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, httpx.BadRequest("Metadata value of " + k + " must be a string")
		}
		lk := strings.ToLower(k)
		if k == "" || len(k) > 255 || len(s) > 1024 || strings.HasPrefix(lk, "hw:") || strings.HasPrefix(lk, "os_") || strings.HasPrefix(lk, "proxcloud_") {
			return nil, httpx.BadRequest("Metadata key " + k + " is not allowed")
		}
		out[k] = s
	}
	if len(out) > maxMetadata {
		return nil, httpx.BadRequest(fmt.Sprintf("At most %d metadata keys are allowed", maxMetadata))
	}
	if len(out) == 0 || h.metadata == nil {
		return out, nil
	}
	opts, err := h.metadata.AvailableOptions(ctx, serviceID, region)
	if err != nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Metadata options are unavailable")
	}
	for _, o := range opts {
		k := strAny(o["key"])
		v, set := out[k]
		if !set {
			continue
		}
		if ed, _ := o["userEditable"].(bool); !ed {
			return nil, httpx.BadRequest("Metadata key " + k + " cannot be set")
		}
		vals, _ := o["options"].([]map[string]any)
		if len(vals) == 0 {
			continue
		}
		allowed := false
		for _, a := range vals {
			allowed = allowed || strAny(a["value"]) == v
		}
		if !allowed {
			return nil, httpx.BadRequest("Metadata value of " + k + " is not one of its options")
		}
	}
	return out, nil
}

// pveNeedsSeed reports whether a server needs a platform seed instead of PVE's drive. PVE's drive sets a password but
// not ssh_pwauth, and a cloud image then refuses that password over ssh, the one place a tenant would type it.
func pveNeedsSeed(es *externalservice.ExternalService, in pveSeedInput) bool {
	return in.UserData != "" || in.Password != "" || len(in.Metadata) > 0 || es.PVEMetadataSettings().VendorData != ""
}

// pveSeedStep returns the CreateServer hook writing the server's seed before its first boot. static
// says the vnet has no DHCP, so only a CD can bring the guest's address (pve/seed.go).
func (h *Handler) pveSeedStep(ps *pve.Service, es *externalservice.ExternalService, proj *Project, in pveSeedInput, static bool) func(ctx context.Context, node string, vmid int) error {
	return func(ctx context.Context, node string, vmid int) error {
		hash := ""
		if in.Password != "" {
			b, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			hash = string(b)
		}
		iid, err := randomHex(8)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		s := &ServerSeed{
			ServiceID: es.ID, ProjectID: proj.ID, ServerID: strconv.Itoa(vmid), Node: node,
			InstanceID: "iid-" + iid, Hostname: seedHostname(in.Name), UserData: in.UserData, LoginUser: in.LoginUser,
			PasswordHash: hash, SSHKeys: in.SSHKeys, Metadata: in.Metadata, CreatedAt: &now,
		}
		settings := es.PVEMetadataSettings()
		base := settings.URL
		if base == "" {
			base = h.apiBaseURL
		}
		s.Mode = seedModeISO
		if !static && base != "" && !settings.ForceISO {
			s.Mode = seedModeURL
		}
		return h.pveWriteSeed(ctx, ps, es, s, strings.TrimRight(base, "/"))
	}
}

// pveWriteSeed renders the seed from the VM's current config and delivers it. base is the seed URL's
// base on a first URL seed ("" on a later write: the VM already holds its token).
func (h *Handler) pveWriteSeed(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService, s *ServerSeed, base string) error {
	ps.SeedArtifacts = h.seeds.artifacts
	return h.seeds.artifacts.Locked(ctx, es.ID, func(ctx context.Context) error { return h.pveWriteSeedLocked(ctx, ps, es, s, base) })
}

func (h *Handler) pveWriteSeedLocked(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService, s *ServerSeed, base string) error {
	unknown, err := h.seeds.artifacts.ServerMutationUnknown(ctx, es.ID, s.ServerID)
	if err != nil {
		return err
	}
	if unknown {
		return fmt.Errorf("seed mutation outcome is unknown; operator reconciliation required")
	}
	if s.readInstanceID != "" {
		var current ServerSeed
		found, err := h.seeds.col.Get(ctx, s.ID, &current)
		if err != nil {
			return err
		}
		if !found || current.Retired || current.InstanceID != s.readInstanceID || current.ISO != s.ISO {
			return fmt.Errorf("seed generation changed during operation; retry")
		}
	}
	vmid, err := strconv.Atoi(s.ServerID)
	if err != nil {
		return fmt.Errorf("seed: bad vmid %q", s.ServerID)
	}
	cfg, err := ps.C.GetVMConfig(ctx, s.Node, vmid)
	if err != nil {
		return err
	}
	if s.readInstanceID != "" && s.Mode == seedModeISO {
		// VMIDs are reusable, including within one project. The existing source
		// must still be positively bound to this VM, not merely found by vmid.
		bound := false
		for key, raw := range cfg {
			if key != "description" {
				if value, ok := raw.(string); ok && strings.HasPrefix(value, s.ISO+",") {
					bound = true
				}
			}
		}
		if s.ISO == "" || !bound {
			return fmt.Errorf("server seed attachment identity changed; operator reconciliation required")
		}
	}
	dns, err := ps.C.NodeDNS(ctx, s.Node)
	if err != nil {
		slog.Warn("seed: node DNS", "node", s.Node, "err", err) // the guest can still reach things by IP
	}
	if s.NetworkConfig, err = pve.RenderNetworkConfig(cfg, dns); err != nil {
		return err
	}
	switch s.Mode {
	case seedModeURL:
		if s.ID == "" {
			token, err := randomHex(32)
			if err != nil {
				return err
			}
			s.ID = hashSeedToken(token)
			if err := h.seeds.put(ctx, s); err != nil {
				return err
			}
			if err := ps.SetSeedURL(ctx, s.Node, vmid, base+"/api/v1/nocloud/"+token+"/"); err != nil {
				return err
			}
			return nil
		}
		return h.seeds.put(ctx, s)
	default:
		if s.ID == "" {
			if s.ID, err = randomHex(32); err != nil {
				return err
			}
		}
		seed, err := pve.RenderSeed(seedSpec(es, s))
		if err != nil {
			return err
		}
		caps, err := ps.C.DetectStorage(ctx)
		if err != nil {
			return err
		}
		storage := pve.PickSeedStorage(caps, s.Node, es.PVEMetadataSettings().ISOStorage)
		if storage == "" {
			return httpx.NewError(http.StatusConflict, http.StatusConflict, "No ISO storage is available on node "+s.Node+" for the server's user-data")
		}
		// Persist the encrypted rebuild source before upload too. A crash after
		// attach must not leave a live CD whose source vanished with the request.
		if err := h.seeds.put(ctx, s); err != nil {
			return err
		}
		volume, err := ps.AttachSeedISO(ctx, s.Node, vmid, storage, seed, pve.SeedArtifact{SeedID: s.ID, ProjectID: s.ProjectID, InstanceID: s.InstanceID})
		if err != nil {
			return err
		}
		s.ISO = volume
		return h.seeds.put(ctx, s)
	}
}

func seedSpec(es *externalservice.ExternalService, s *ServerSeed) pve.SeedSpec {
	return pve.SeedSpec{
		InstanceID: s.InstanceID, Hostname: s.Hostname, UserData: s.UserData,
		OperatorVendorData: es.PVEMetadataSettings().VendorData,
		PasswordHash:       s.PasswordHash, LoginUser: s.LoginUser, SSHKeys: s.SSHKeys, Metadata: s.Metadata,
		Network: s.NetworkConfig,
	}
}

// pveReseed re-delivers a server's seed after its NICs changed. Best-effort: it only matters at the
// next boot, and the address itself stays pinned by ipfilter.
func (h *Handler) pveReseed(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService, projectID, serverID, node string) {
	if h.seeds == nil {
		return
	}
	s, err := h.seeds.ForServer(ctx, es.ID, projectID, serverID)
	if err == nil && s != nil {
		s.Node = node
		err = h.pveWriteSeed(ctx, ps, es, s, "")
	}
	if err != nil {
		slog.Error("reseed", "server", serverID, "err", err)
	}
}

// pveReleaseSeed revokes the server's token and removes its CD.
func (h *Handler) pveReleaseSeed(ctx context.Context, ps *pve.Service, serviceID, serverID string) {
	if h.seeds == nil {
		return
	}
	var rows []ServerSeed
	err := h.seeds.col.Find(ctx, pgdoc.M{"serviceId": serviceID, "serverId": serverID}, &rows)
	if err == nil {
		h.pveDropSeeds(ctx, ps, rows)
	} else {
		slog.Error("release seed", "server", serverID, "err", err)
	}
}

// staleSeed: past the create grace, the seed's VM has no row, or the row is another project's because
// the vmid was reused.
func staleSeed(s *ServerSeed, row *cloud.CloudResource, now time.Time) bool {
	if s.CreatedAt != nil && now.Sub(*s.CreatedAt) <= seedOrphanGrace {
		return false
	}
	return row == nil || row.ProjectID != s.ProjectID
}

// DropStaleSeeds is the sync's cleanup: a VM destroyed outside the portal never runs pveReleaseSeed, and
// its seed would hold its user-data, token and CD forever.
func (h *Handler) DropStaleSeeds(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService) error {
	if h.seeds == nil {
		return nil
	}
	var rows []ServerSeed
	if err := h.seeds.col.Find(ctx, pgdoc.M{"serviceId": es.ID}, &rows); err != nil {
		return err
	}
	now := time.Now().UTC()
	var stale []ServerSeed
	for i := range rows {
		row, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, rows[i].ServerID)
		if err != nil {
			return err
		}
		if rows[i].Retired || staleSeed(&rows[i], row, now) {
			// Local cache absence is only a candidate, not authority to revoke a
			// live seed or erase its source. Reused vmids are retained conservatively.
			if !rows[i].Retired {
				if ps.SeedReader == nil {
					continue
				}
				exists, err := ps.SeedReader.SeedVMExists(ctx, rows[i].ServerID)
				if err != nil || exists {
					continue
				}
			}
			stale = append(stale, rows[i])
		}
	}
	h.pveDropSeeds(ctx, ps, stale, false)
	// Includes upload/attach failures and superseded CDs with no active-seed
	// row; never enumerate/adopt files on shared storage by their prefix.
	ps.SeedArtifacts = h.seeds.artifacts
	artifacts, err := h.seeds.artifacts.Pending(ctx, es.ID)
	if err != nil {
		return err
	}
	for _, a := range artifacts {
		if err := ps.DeleteSeedVolume(ctx, a.Node, a.Volume); err != nil {
			slog.Warn("seed artifact cleanup pending", "artifact", a.ID, "reason", err)
		}
	}
	return nil
}

// pveReleaseProjectSeeds is the project-teardown variant.
func (h *Handler) pveReleaseProjectSeeds(ctx context.Context, ps *pve.Service, serviceID, projectID string) error {
	if h.seeds == nil {
		return nil
	}
	var rows []ServerSeed
	if err := h.seeds.col.Find(ctx, pgdoc.M{"serviceId": serviceID, "projectId": projectID}, &rows); err != nil {
		return err
	}
	h.pveDropSeeds(ctx, ps, rows)
	return nil
}

func (h *Handler) pveDropSeeds(ctx context.Context, ps *pve.Service, rows []ServerSeed, explicit ...bool) {
	ps.SeedArtifacts = h.seeds.artifacts
	for _, seed := range rows {
		err := h.seeds.artifacts.Locked(ctx, ps.ServiceID, func(ctx context.Context) error {
			// Re-read under the same lock as writers: never retire a seed that a
			// concurrent rebuild/reseed replaced since this candidate was listed.
			var s ServerSeed
			found, err := h.seeds.col.Get(ctx, seed.ID, &s)
			if err != nil || !found {
				return err
			}
			if s.InstanceID != seed.InstanceID || s.ISO != seed.ISO {
				return nil
			}
			if len(explicit) > 0 && !explicit[0] && !s.Retired {
				// Candidate selection raced a new create/rebuild: repeat authority
				// under the writer lock before changing active source state.
				if ps.SeedReader == nil {
					return fmt.Errorf("seed cleanup pending: complete reader unavailable")
				}
				exists, err := ps.SeedReader.SeedVMExists(ctx, s.ServerID)
				if err != nil {
					return err
				}
				if exists {
					return nil
				}
			}
			// Preserve ciphertext verbatim. Calling put here would encrypt an
			// already encrypted source, making an eventual recovery unreadable.
			if _, err := h.seeds.col.SetByID(ctx, s.ID, pgdoc.M{"retired": true}, nil); err != nil {
				return err
			}
			artifacts, err := h.seeds.artifacts.ForSeed(ctx, ps.ServiceID, s.ID)
			if err != nil {
				return err
			}
			seen := map[string]bool{}
			var pending error
			for _, a := range artifacts {
				seen[a.Volume] = true
				if err := ps.DeleteSeedVolume(ctx, a.Node, a.Volume); err != nil {
					pending = err
				}
			}
			// Legacy source or an old source.ISO still gets the same gate; it
			// cannot be adopted into the journal just because the DB names it.
			if s.ISO != "" && !seen[s.ISO] {
				if err := ps.DeleteSeedVolume(ctx, s.Node, s.ISO); err != nil {
					pending = err
				}
			}
			if pending != nil {
				return pending
			}
			_, err = h.seeds.col.DeleteByID(ctx, s.ID)
			return err
		})
		if err != nil {
			slog.Warn("seed cleanup pending; encrypted source retained", "server", seed.ServerID, "reason", err)
		}
	}
}

// nocloudFile serves one file of a URL seed. cloud-init needs a 200 for meta-data and user-data; a
// 404 on the optional two means "none".
func (h *Handler) nocloudFile(w http.ResponseWriter, r *http.Request) {
	if h.seeds == nil {
		h.fail(w, httpx.NotFound("Not found"))
		return
	}
	s, err := h.seeds.byToken(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if s == nil || s.Retired || s.Mode != seedModeURL {
		h.fail(w, httpx.NotFound("Not found"))
		return
	}
	// A server deleted outside the portal: the sync archived its row, so its token is dead. The grace
	// covers a first boot racing the create's own insert.
	if s.CreatedAt != nil && time.Since(*s.CreatedAt) > seedOrphanGrace {
		if row, err := h.cloud.FindByServiceIDAndExternalID(r.Context(), s.ServiceID, s.ServerID); err == nil && staleSeed(s, row, time.Now().UTC()) {
			// Refuse delivery on cache mismatch, but do not destroy the encrypted
			// source: only the complete live-view cleanup can prove it stale.
			h.fail(w, httpx.NotFound("Not found"))
			return
		}
	}
	es, err := h.esSvc.Get(r.Context(), s.ServiceID)
	if err != nil || es == nil {
		h.fail(w, httpx.NotFound("Not found"))
		return
	}
	seed, err := pve.RenderSeed(seedSpec(es, s))
	if err != nil {
		h.fail(w, err)
		return
	}
	body, ok := seed.Files()[chi.URLParam(r, "file")]
	if !ok {
		h.fail(w, httpx.NotFound("Not found"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}
