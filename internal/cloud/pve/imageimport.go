package pve

// imageimport.go — a customer image from a URL: the portal fetches it and streams it to the provider's import store
// (content=import), a VM takes it with import-from, and that VM becomes the project's template.
//
// A node never sees the tenant's URL. PVE's download-url follows redirects with no way to turn that off, and resolves the
// name again, so a server answering the portal with an image and the node with a 302 would reach the node's network. The
// portal's own fetch checks each address it dials (pkg/safeurl), and PVE gets nothing to fetch.
//
// PVE checks an uploaded image as untrusted and refuses a backing file, on upload and again on import-from. raw is not
// offered: no check can tell a disk image from any other bytes.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// ImportTimeout bounds one import, from the first byte fetched to the template.
	ImportTimeout = time.Hour
	// MaxImportDiskGiB bounds an image's virtual size: a small qcow2 can claim a huge disk.
	MaxImportDiskGiB = 200 // ponytail: fixed limit, make it a provider setting if a site needs bigger images
)

// importSuffix makes the downloaded file's name unique (test seam).
var importSuffix = func() string { return randBase36(8) }

// ImportFormats are the formats an image import accepts.
var ImportFormats = map[string]bool{"qcow2": true, "vmdk": true}

// ImageImport is one import. Node has both stores active.
type ImageImport struct {
	Name, Format                string
	Source                      io.Reader // the image file, SourceSize bytes
	SourceSize                  int64
	Checksum, ChecksumAlgorithm string // optional, verified by PVE
	Node                        string
	ImportStorage, DiskStorage  string
	HeadroomPct                 float64
	// CheckDisk refuses a disk of this virtual size, which only the uploaded file can tell.
	CheckDisk func(ctx context.Context, sizeBytes int64) error
}

// LockImageImport allows one import at a time per project: each holds a portal connection and a node's temporary space
// for as long as it runs. The release must be called. Busy = ConflictError.
func (s *Service) LockImageImport(ctx context.Context, projectID string) (func(), error) {
	if s.HolderLock == nil {
		return func() {}, nil
	}
	release, acquired, err := s.HolderLock(ctx, "pve-image-import-"+projectID, ImportTimeout+time.Minute, 0)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, conflictf("Another image import is running in this project. Try again when it has finished")
	}
	return release, nil
}

// UploadImportFile streams an image into storage's import directory (POST .../upload, multipart) and returns the task
// that checks and copies it. Needs Datastore.AllocateTemplate on the storage. PVE refuses an upload with no
// Content-Length, so size must be exact. A client of its own: the shared timeout would bound the whole body (ctx bounds
// this), and a connection reused from an earlier call can be closed under a body that cannot be sent again.
func (c *Client) UploadImportFile(ctx context.Context, node, storage, filename string, src io.Reader, size int64, checksum, algorithm string) (UPID, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := [][2]string{{"content", "import"}}
	if checksum != "" {
		// pveproxy's parser wants these in this order: content, checksum-algorithm, checksum, the file (400 otherwise).
		fields = append(fields, [2]string{"checksum-algorithm", algorithm}, [2]string{"checksum", checksum})
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return "", err
		}
	}
	if _, err := w.CreateFormFile("filename", filename); err != nil {
		return "", err
	}
	head := bytes.Clone(buf.Bytes())
	buf.Reset()
	if err := w.Close(); err != nil {
		return "", err
	}
	path := "/nodes/" + node + "/storage/" + url.PathEscape(storage) + "/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL()+path, io.MultiReader(bytes.NewReader(head), src, &buf))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(head)) + size + int64(buf.Len())
	t := c.http.Transport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	var upid UPID
	if err := c.sendBody(&http.Client{Transport: t}, req, w.FormDataContentType(), &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// uploadSpace is the room for an upload on the API node: pveproxy spools the body to /var/tmp there before PVE sees it,
// whichever node it is for, and that is the node's root disk on a stock install.
func (c *Client) uploadSpace(ctx context.Context) (StorageNodeState, error) {
	var rows []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Local int    `json:"local"`
	}
	if err := c.get(ctx, "/cluster/status", nil, &rows); err != nil {
		return StorageNodeState{}, err
	}
	for _, r := range rows {
		if r.Type == "node" && r.Local == 1 {
			var st struct {
				RootFS struct {
					Total int64 `json:"total"`
					Avail int64 `json:"avail"`
				} `json:"rootfs"`
			}
			if err := c.get(ctx, "/nodes/"+url.PathEscape(r.Name)+"/status", nil, &st); err != nil {
				return StorageNodeState{}, err
			}
			return StorageNodeState{Active: true, Total: st.RootFS.Total, Avail: st.RootFS.Avail}, nil
		}
	}
	return StorageNodeState{}, fmt.Errorf("pve: the API node is not in the cluster status")
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
// no VM is left, and neither is the uploaded file while the token may delete it.
// ponytail: a crash of the portal mid-import leaves the file and a pc-img-build- VM; a sweep can take them if it ever happens.
func (s *Service) ImportImage(ctx context.Context, projectID string, req ImageImport) (int, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, ImportTimeout)
	defer cancel()
	// ponytail: per project, so several projects' uploads can still add up on this disk; a provider-wide limit if that shows.
	space, err := s.C.uploadSpace(ctx)
	if err != nil {
		if IsForbidden(err) {
			return 0, 0, conflictf("The provider's API token cannot read the node's disk space (it needs Sys.Audit on /nodes)")
		}
		return 0, 0, err
	}
	if !StorageHasRoom(space, req.SourceSize, req.HeadroomPct) {
		slog.Warn("pve image import: the API node has no room for the upload", "project", projectID, "bytes", req.SourceSize, "avail", space.Avail)
		return 0, 0, conflictf("This image is too large to upload right now. Try again later, or use a smaller image")
	}
	file := "pc-import-" + projectID + "-" + importSuffix() + "." + req.Format
	up, err := s.C.UploadImportFile(ctx, req.Node, req.ImportStorage, file, req.Source, req.SourceSize, req.Checksum, req.ChecksumAlgorithm)
	if err != nil {
		if IsForbidden(err) {
			return 0, 0, conflictf("The provider's API token cannot import images (it needs Datastore.AllocateTemplate on storage %s)", req.ImportStorage)
		}
		return 0, 0, err
	}
	if err := s.C.WaitTask(ctx, up); err != nil {
		slog.Warn("pve image import: upload", "project", projectID, "err", err)
		return 0, 0, conflictf("The image is not a valid %s file, or does not match its checksum (a backing file is not allowed)", req.Format)
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
	if req.CheckDisk != nil {
		if err := req.CheckDisk(ctx, size); err != nil {
			return 0, 0, err
		}
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

// dropImportFile removes the uploaded file. It needs Datastore.Allocate on the import store, which the role does
// not hold anywhere else (docs/proxmox-permissions.md); without it the image still works, the file stays, and Test
// connection warns.
func (s *Service) dropImportFile(ctx context.Context, node, volid string) {
	ctx = context.WithoutCancel(ctx)
	store, vol := slotStorage(volid), volid[len(slotStorage(volid))+1:]
	if err := s.C.DeleteStorageVolume(ctx, node, store, vol); err != nil {
		slog.Error("pve image import: the uploaded file stays on the store", "volume", volid, "err", err)
	}
}
