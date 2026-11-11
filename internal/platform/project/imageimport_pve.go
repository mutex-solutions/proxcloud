package project

// imageimport_pve.go — IMAGE create from a URL (pve/imageimport.go).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
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

// spoolImage reads a body of unstated size to a temporary file, since PVE takes no upload without a length. The caller
// removes the file. Over maxBytes: nil.
func spoolImage(body io.Reader, maxBytes int64) (*os.File, int64, error) {
	f, err := os.CreateTemp("", "pc-image-*")
	if err != nil {
		return nil, 0, err
	}
	n, err := io.Copy(f, io.LimitReader(body, maxBytes+1))
	if err == nil && n <= maxBytes {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil || n > maxBytes {
		f.Close()
		os.Remove(f.Name())
		return nil, n, err
	}
	return f, n, nil
}

// An import holds the provider's upload lock while it reads its source (pve.lockImportUpload), so a tenant's slow URL must
// not get to hold it for the whole of pve.ImportTimeout: a source that goes silent, or falls behind a floor, ends the fetch.
// ponytail: a source just above the floor still holds the lock until ImportTimeout, at the price of really sending that
// much; the byte budget on lockImportUpload is what would end that.
var (
	importStall         = time.Minute     // nothing read for this long
	importGrace         = 2 * time.Minute // what a source gets on top of importMinRate
	errImportSourceSlow = errors.New("image source too slow")
)

const importMinRate = 2 << 20 // bytes a second the source has to average

// importBody ends a fetch, through cancel, when a read outlasts what the source has earned. Only time in a read counts, so a
// slow PVE on the other side does not count against the source.
type importBody struct {
	r     io.Reader
	timer *time.Timer
	n     int64
	spent time.Duration
}

func newImportBody(r io.Reader, cancel context.CancelCauseFunc) *importBody {
	t := time.AfterFunc(importStall, func() { cancel(errImportSourceSlow) })
	t.Stop()
	return &importBody{r: r, timer: t}
}

func (b *importBody) Read(p []byte) (int, error) {
	start := time.Now()
	b.timer.Reset(min(importStall, importGrace+time.Duration(b.n/importMinRate)*time.Second-b.spent))
	n, err := b.r.Read(p)
	b.timer.Stop()
	b.n += int64(n)
	b.spent += time.Since(start)
	return n, err
}

func gibUp(b int64) int { return int((b + 1<<30 - 1) >> 30) }

// pveImportImage makes a project template from a URL. Body: {name, url, format?, checksum?, checksumAlgorithm?, profile?}.
// A client that goes away cancels nothing: the import finishes and the sampler adds the image.
func (h *Handler) pveImportImage(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, data map[string]any) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), pve.ImportTimeout)
	defer cancel()
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
	ps, ok := h.pveService(w, es)
	if !ok {
		return
	}
	caps, err := ps.C.DetectStorage(ctx)
	if err != nil {
		h.fail(w, upstreamErr(computeBackend, err))
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
	release, err := ps.LockImageImport(ctx, proj.ID)
	if err != nil {
		h.fail(w, pveVolumeErr(err))
		return
	}
	defer release()

	fetchCtx, endFetch := context.WithCancelCause(ctx)
	defer endFetch(nil)
	sourceSlow := func() bool {
		if !errors.Is(context.Cause(fetchCtx), errImportSourceSlow) {
			return false
		}
		conflict(fmt.Sprintf("The server at this URL stopped sending, or sent too slowly: an import needs %d MiB/s from it", importMinRate>>20))
		return true
	}
	src, err := safeurl.Open(fetchCtx, strAny(data["url"]), importAlso(es.PVENetworkConfig().BlockedCIDRs))
	if err != nil {
		var refusal *safeurl.Refusal
		if errors.As(err, &refusal) {
			h.fail(w, httpx.BadRequest(refusal.Msg))
			return
		}
		h.fail(w, upstreamErr("image URL", err))
		return
	}
	defer src.Body.Close()
	body, size := io.Reader(newImportBody(src.Body, endFetch)), src.Size
	if size < 0 {
		maxGiB := es.PVEImportUnknownSizeMaxGiB()
		if maxGiB == 0 {
			conflict("The server did not say how large this image is, and this provider only takes images of a known size")
			return
		}
		f, n, err := spoolImage(body, int64(maxGiB)<<30)
		if err != nil {
			if sourceSlow() {
				return
			}
			h.fail(w, upstreamErr("image URL", err))
			return
		}
		if f == nil {
			conflict(fmt.Sprintf("An image of unknown size can be at most %d GiB on this provider", maxGiB))
			return
		}
		defer os.Remove(f.Name())
		defer f.Close()
		body, size = f, n
	}
	if size == 0 {
		conflict("The file at this URL is empty")
		return
	}
	// The file is at most about its disk, so this refuses early what the disk check below would refuse anyway.
	if size > pve.MaxImportDiskGiB<<30 {
		conflict(fmt.Sprintf("The image is %d GiB; the limit is %d GiB", size>>30, pve.MaxImportDiskGiB))
		return
	}
	if herr := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{DiskGb: gibUp(size)}); herr != nil {
		h.fail(w, herr)
		return
	}
	if !pve.StorageHasRoom(importCap.Nodes[node], size, es.PVEStorageHeadroomPct()) {
		conflict(fmt.Sprintf("Storage %s has no room for this image right now", store))
		return
	}
	started := time.Now().UTC()
	vmid, disk, err := ps.ImportImage(ctx, proj.ID, pve.ImageImport{
		Name: name, Format: format, Source: body, SourceSize: size, Checksum: checksum, ChecksumAlgorithm: algorithm,
		Node: node, ImportStorage: store, DiskStorage: profile.StorageFor(node), HeadroomPct: es.PVEStorageHeadroomPct(),
		CheckDisk: func(ctx context.Context, sizeBytes int64) error {
			if herr := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{DiskGb: gibUp(sizeBytes)}); herr != nil {
				return herr
			}
			caps, err := ps.C.DetectStorage(ctx)
			if err != nil {
				return err
			}
			if !pve.StorageHasRoom(profile.Placement(caps)[node], sizeBytes, es.PVEStorageHeadroomPct()) {
				return httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf("Storage profile %s has no room for this image's %d GiB disk right now", slug, gibUp(sizeBytes)))
			}
			return nil
		},
	})
	if err != nil {
		slog.Warn("pve image import", "project", proj.ID, "err", err)
		if sourceSlow() {
			return
		}
		var herr *httpx.HTTPError
		if errors.As(err, &herr) {
			h.fail(w, herr)
			return
		}
		h.fail(w, pveVolumeErr(err))
		return
	}
	h.pveSaveImageRow(w, r, u, proj, es, name, vmid, disk, started)
}
