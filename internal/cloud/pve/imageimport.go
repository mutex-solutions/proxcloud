package pve

// imageimport.go — a customer image from a URL: a node downloads it to the provider's import store (content=import),
// a VM takes it with import-from, and that VM becomes the project's template. The caller has checked the URL.
//
// PVE checks a downloaded image as untrusted and refuses a backing file, on download and again on import-from. raw is
// not offered: any bytes are a valid raw image, so a URL a node resolved differently than the portal did could put a
// response from the internal network on a tenant's disk.

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"time"
)

const (
	importTimeout = time.Hour
	// MaxImportDiskGiB bounds an image's virtual size: a small qcow2 can claim a huge disk.
	MaxImportDiskGiB = 200 // ponytail: fixed limit, make it a provider setting if a site needs bigger images
)

// importSuffix makes the downloaded file's name unique (test seam).
var importSuffix = func() string { return randBase36(8) }

// ImportFormats are the formats an image import accepts.
var ImportFormats = map[string]bool{"qcow2": true, "vmdk": true}

// ImageImport is one import. Node has both stores active.
type ImageImport struct {
	Name, URL, Format           string
	Checksum, ChecksumAlgorithm string // optional, verified by the node
	Node                        string
	ImportStorage, DiskStorage  string
}

// DownloadURL has a node fetch url into storage's import directory. Needs Datastore.AllocateTemplate on the
// storage, plus Sys.Audit and Sys.Modify on / or Sys.AccessNetwork on the node.
func (c *Client) DownloadURL(ctx context.Context, node, storage, filename, fileURL, checksum, algorithm string) (UPID, error) {
	return c.postTask(ctx, "/nodes/"+node+"/storage/"+url.PathEscape(storage)+"/download-url", formVals{
		"content": "import", "filename": filename, "url": fileURL, "checksum": checksum, "checksum-algorithm": algorithm,
	}.values())
}

// VolumeSizeBytes reads a volume's virtual size.
func (c *Client) VolumeSizeBytes(ctx context.Context, node, storage, volid string) (int64, error) {
	var out struct {
		Size int64 `json:"size"`
	}
	path := "/nodes/" + node + "/storage/" + url.PathEscape(storage) + "/content/" + url.PathEscape(volid)
	if err := c.get(ctx, path, nil, &out); err != nil {
		return 0, err
	}
	return out.Size, nil
}

// ImportImage runs an import to a template in the project's pool and returns its vmid and disk size. Whatever fails,
// no VM is left, and neither is the downloaded file while the token may delete it.
// ponytail: a crash of the portal mid-import leaves the file and a pc-img-build- VM; a sweep can take them if it ever happens.
func (s *Service) ImportImage(ctx context.Context, projectID string, req ImageImport) (int, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	file := "pc-import-" + projectID + "-" + importSuffix() + "." + req.Format
	up, err := s.C.DownloadURL(ctx, req.Node, req.ImportStorage, file, req.URL, req.Checksum, req.ChecksumAlgorithm)
	if err != nil {
		if IsForbidden(err) {
			return 0, 0, conflictf("The provider's API token cannot import images (it needs Datastore.AllocateTemplate on storage %s, and Sys.AccessNetwork on the node or Sys.Audit and Sys.Modify on /)", req.ImportStorage)
		}
		return 0, 0, err
	}
	if err := s.C.WaitTask(ctx, up); err != nil {
		// The task log is the node's view of the URL: logged, not shown.
		slog.Warn("pve image import: download", "project", projectID, "err", err)
		return 0, 0, conflictf("The image could not be downloaded, or it is not a valid %s file (a backing file is not allowed)", req.Format)
	}
	volid := req.ImportStorage + ":import/" + file
	defer s.dropImportFile(ctx, req.Node, volid)

	size, err := s.C.VolumeSizeBytes(ctx, req.Node, req.ImportStorage, volid)
	if err != nil {
		return 0, 0, err
	}
	if size > MaxImportDiskGiB<<30 {
		return 0, 0, conflictf("The image's disk is %d GiB; the limit is %d GiB", size>>30, MaxImportDiskGiB)
	}
	vmid, err := s.C.NextID(ctx)
	if err != nil {
		return 0, 0, err
	}
	// Empty first: the disk and CD need a VM already in the pool. Named from the start, or the sampler bills the
	// VM as a server for as long as import-from runs (the config, name included, lands when the task ends).
	up, err = s.C.CreateVM(ctx, req.Node, map[string]string{"vmid": strconv.Itoa(vmid), "pool": s.PoolFor(projectID), "name": imageBuildPrefix + req.Name})
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		return 0, 0, err
	}
	fail := func(err error) (int, int64, error) {
		s.destroyBestEffort(context.WithoutCancel(ctx), req.Node, vmid)
		return 0, 0, err
	}
	// The same shape as a pipeline template (images/build.py), without the NIC: every server gets its own on create.
	up, err = s.C.SetVMConfig(ctx, req.Node, vmid, map[string]string{
		"ostype": "l26", "cores": "2", "memory": "2048", "cpu": "host", "agent": "1",
		"scsihw": "virtio-scsi-single", "serial0": "socket", "vga": "serial0", "tags": TagManaged, "boot": "order=scsi0",
		"scsi0": fmt.Sprintf("%s:0,discard=on,import-from=%s", req.DiskStorage, volid),
		"ide2":  req.DiskStorage + ":cloudinit",
	})
	if err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		slog.Warn("pve image import: import-from", "project", projectID, "vmid", vmid, "err", err)
		return fail(conflictf("The image could not be imported: it is not a valid %s disk, or storage %s refused it", req.Format, req.DiskStorage))
	}
	if up, err = s.C.ToTemplate(ctx, req.Node, vmid); err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		return fail(err)
	}
	if up, err = s.C.SetVMConfig(ctx, req.Node, vmid, map[string]string{"name": req.Name}); err == nil {
		err = s.C.WaitTask(ctx, up)
	}
	if err != nil {
		return fail(err)
	}
	return vmid, size, nil
}

// dropImportFile removes the downloaded file. It needs Datastore.Allocate on the import store, which the role does
// not hold anywhere else (docs/proxmox-permissions.md); without it the image still works, the file stays, and Test
// connection warns.
func (s *Service) dropImportFile(ctx context.Context, node, volid string) {
	ctx = context.WithoutCancel(ctx)
	store, vol := slotStorage(volid), volid[len(slotStorage(volid))+1:]
	if err := s.C.DeleteStorageVolume(ctx, node, store, vol); err != nil {
		slog.Error("pve image import: the downloaded file stays on the store", "volume", volid, "err", err)
	}
}
