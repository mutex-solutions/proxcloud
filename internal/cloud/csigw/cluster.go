package csigw

// cluster.go is the gateway's side of a cluster's life: the controller config minted at create, and the release after
// a delete that revokes the credential and removes the disks the cluster still has.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// ControllerVMID is the plugin's default features.controllerVmID: every disk a cluster creates is vm-9999-pvc-….
const ControllerVMID = 9999

// Endpoint is the gateway as the clusters' controllers reach it: its URL (https://host:port) and the PEM CA its
// certificate is checked against.
type Endpoint struct {
	URL string
	CA  string
}

// ControllerSecret mints the cluster's credential and returns the data of the Secret its proxmox-csi controller mounts
// at /etc/proxmox: config.yaml, pointing at the gateway with the cluster's token, and the CA it trusts. The secret
// exists only in this answer.
func (s *Store) ControllerSecret(ctx context.Context, c Credential, ep Endpoint) (map[string]string, error) {
	if ep.URL == "" || ep.CA == "" {
		return nil, errors.New("csigw: the gateway needs a URL and a CA")
	}
	c.ControllerVMID = ControllerVMID
	tokenID, secret, err := s.MintCredential(ctx, c)
	if err != nil {
		return nil, err
	}
	cfg, err := json.Marshal(map[string]any{
		"features": map[string]any{"provider": "capmox", "controllerVmID": c.ControllerVMID},
		"clusters": []any{map[string]any{
			"url":          strings.TrimRight(ep.URL, "/") + "/api2/json",
			"ca_file":      "/etc/proxmox/ca.crt",
			"token_id":     tokenID,
			"token_secret": secret,
			"region":       c.Region,
		}},
	})
	if err != nil {
		return nil, err
	}
	// JSON is YAML.
	return map[string]string{"config.yaml": string(cfg), "ca.crt": ep.CA}, nil
}

// ReleaseCluster runs once a deleted cluster's workers are gone: it revokes the credential, then deletes every disk the
// registry still holds for the cluster and marks its row deleted. gw builds the gateway's client of a compute provider.
// A provider that no longer exists has nothing left to delete on. Any other error leaves the remaining rows as they are,
// so the caller's sweep runs it again: a disk still attached somewhere answers in-use until it is not.
// A PersistentVolume with the Retain policy goes too: with the cluster gone, nothing can mount it again.
func (s *Store) ReleaseCluster(ctx context.Context, clusterID string, gw func(context.Context, string) (*pve.Client, error)) error {
	if err := s.RevokeCredential(ctx, clusterID); err != nil {
		return err
	}
	var rows []Volume
	if err := s.vols.Find(ctx, pgdoc.M{"clusterId": clusterID, "state": pgdoc.M{"$ne": StateDeleted}}, &rows); err != nil {
		return err
	}
	clients := map[string]*pve.Client{}
	for i := range rows {
		v := &rows[i]
		cl, ok := clients[v.ComputeServiceID]
		if !ok {
			var err error
			cl, err = gw(ctx, v.ComputeServiceID)
			if errors.Is(err, externalservice.ErrNotFound) {
				cl = nil
			} else if err != nil {
				return err
			}
			clients[v.ComputeServiceID] = cl
		}
		if cl != nil {
			if err := deleteDisk(ctx, cl, v); err != nil {
				return fmt.Errorf("csigw: delete %s: %w", v.VolID, err)
			}
		}
		if err := s.markDeleted(ctx, v.ID); err != nil {
			return err
		}
	}
	return nil
}

// deleteDisk removes v's disk when its store still lists it. Asking first keeps "already gone" from being read off an
// error message.
func deleteDisk(ctx context.Context, cl *pve.Client, v *Volume) error {
	sizes, err := diskSizes(ctx, cl, v.Node, v.Storage)
	if err != nil {
		return err
	}
	if _, there := sizes[v.VolID]; !there {
		return nil
	}
	_, disk, _ := strings.Cut(v.VolID, ":")
	return cl.DeleteStorageVolume(ctx, v.Node, v.Storage, disk)
}

// diskSizes lists the disks store s holds on node, volid → bytes.
func diskSizes(ctx context.Context, cl *pve.Client, node, s string) (map[string]int64, error) {
	list, err := cl.StorageContents(ctx, node, s, "images", 0)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(list))
	for _, c := range list {
		out[c.VolID] = c.Size
	}
	return out, nil
}

func (s *Store) markDeleted(ctx context.Context, id string) error {
	_, err := s.vols.SetByID(ctx, id, pgdoc.M{"state": StateDeleted, "deletedAt": time.Now().UTC()}, nil)
	return err
}
