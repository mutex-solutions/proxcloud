package project

// imageimport_pve.go — IMAGE create from a URL (pve/imageimport.go).

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/safeurl"
)

var (
	imageNameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,38}[a-zA-Z0-9])?$`)
	checksumRe  = regexp.MustCompile(`^[0-9a-fA-F]{64}([0-9a-fA-F]{64})?$`)
)

// validateImageImport checks what the tenant sent, before anything leaves the portal. The node verifies the
// checksum; here it only must not be able to reach PVE as anything else.
func validateImageImport(name, format, checksum, algorithm string) *httpx.HTTPError {
	if !imageNameRe.MatchString(name) {
		return httpx.BadRequest("An image name uses 1 to 40 letters, digits and dashes, and starts and ends with a letter or digit")
	}
	if !pve.ImportFormats[format] {
		return httpx.BadRequest("The format must be qcow2 or vmdk")
	}
	if checksum == "" && algorithm == "" {
		return nil
	}
	want := map[string]int{"sha256": 64, "sha512": 128}[algorithm]
	if want == 0 || len(checksum) != want || !checksumRe.MatchString(checksum) {
		return httpx.BadRequest("The checksum must be a sha256 or sha512 hex digest, with checksumAlgorithm saying which")
	}
	return nil
}

// importAlso is the check the operator adds to safeurl's: the networks tenants must not reach.
func importAlso(blocked []string) func(netip.Addr) bool {
	prefixes := make([]netip.Prefix, 0, len(blocked))
	for _, c := range blocked {
		if p, err := netip.ParsePrefix(c); err == nil {
			prefixes = append(prefixes, p)
		}
	}
	return func(ip netip.Addr) bool {
		ip = ip.Unmap()
		for _, p := range prefixes {
			if p.Contains(ip) {
				return true
			}
		}
		return false
	}
}

// pickImportNode is the node with both stores active and the most room on the disk store; ties go by name.
func pickImportNode(disk map[string]pve.StorageNodeState, importCap pve.StorageCapability) string {
	nodes := make([]string, 0, len(disk))
	for n := range disk {
		if importCap.Nodes[n].Active {
			nodes = append(nodes, n)
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		if disk[nodes[i]].Avail != disk[nodes[j]].Avail {
			return disk[nodes[i]].Avail > disk[nodes[j]].Avail
		}
		return nodes[i] < nodes[j]
	})
	if len(nodes) == 0 {
		return ""
	}
	return nodes[0]
}

// pveImportImage makes a project template from a URL. Body: {name, url, format?, checksum?, checksumAlgorithm?, profile?}.
func (h *Handler) pveImportImage(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, data map[string]any) {
	ctx := r.Context()
	conflict := func(msg string) { h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, msg)) }
	store := es.PVEImportStorage()
	if store == "" {
		conflict("This provider does not offer image import")
		return
	}
	name, format := strings.TrimSpace(strAny(data["name"])), strAny(data["format"])
	if format == "" {
		format = "qcow2"
	}
	checksum, algorithm := strAny(data["checksum"]), strAny(data["checksumAlgorithm"])
	if herr := validateImageImport(name, format, checksum, algorithm); herr != nil {
		h.fail(w, herr)
		return
	}
	slug := strAny(data["profile"])
	if slug == "" {
		slug = es.PVEDefaultStorageProfile()
	}
	profile, ok := es.PVEStorageProfiles()[slug]
	if !ok || !profile.BootDisk() {
		conflict("Image import needs a storage profile that takes boot disks")
		return
	}
	target, err := safeurl.Check(ctx, strAny(data["url"]), importAlso(es.PVENetworkConfig().BlockedCIDRs))
	if err != nil {
		var refusal *safeurl.Refusal
		if errors.As(err, &refusal) {
			h.fail(w, httpx.BadRequest(refusal.Msg))
			return
		}
		h.fail(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, err.Error()))
		return
	}
	ps, ok := h.pveService(w, es)
	if !ok {
		return
	}
	caps, err := ps.C.DetectStorage(ctx)
	if err != nil {
		h.fail(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, err.Error()))
		return
	}
	importCap := caps[store]
	if !importCap.HasContent("import") {
		conflict(fmt.Sprintf("Storage %s does not allow import content", store))
		return
	}
	node := pickImportNode(profile.Placement(caps), importCap)
	if node == "" {
		conflict(fmt.Sprintf("No node has both storage %s and storage profile %s active", store, slug))
		return
	}
	if target.Size > 0 && !pve.StorageHasRoom(importCap.Nodes[node], target.Size, es.PVEStorageHeadroomPct()) {
		conflict(fmt.Sprintf("Storage %s has no room for this image right now", store))
		return
	}
	started := time.Now().UTC()
	vmid, size, err := ps.ImportImage(ctx, proj.ID, pve.ImageImport{
		Name: name, URL: target.URL, Format: format, Checksum: checksum, ChecksumAlgorithm: algorithm,
		Node: node, ImportStorage: store, DiskStorage: profile.StorageFor(node),
	})
	if err != nil {
		slog.Warn("pve image import", "project", proj.ID, "err", err)
		h.fail(w, pveVolumeErr(err))
		return
	}
	h.pveSaveImageRow(w, r, u, proj, es, name, vmid, size, started)
}
