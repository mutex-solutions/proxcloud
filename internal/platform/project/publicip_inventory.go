package project

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// ReleaseIfUnchanged is ONLY the orphan reconciler's exact-generation release.
// The caller has just proved VM absence with the configured provider credential
// and archived expectedServer. Neither a timestamp nor the VMID alone is proof.
// The external-id lock also used by cloud.Upsert/InsertIfAbsent keeps a new cache
// generation from appearing between the absence check and allocation deletion.
func (r *PublicIPRepo) ReleaseIfUnchanged(ctx context.Context, expected PublicIP, expectedServer *cloud.CloudResource) (bool, error) {
	if expectedServer == nil || expectedServer.ID == "" || expectedServer.ServiceID != expected.ServiceID || expectedServer.ProjectID != expected.ProjectID || expectedServer.ExternalID != expected.ServerID || expected.BeforeRow || expected.NIC == ReservedNIC {
		return false, nil
	}
	db := r.col.DB()
	for _, name := range []string{"cloudResource", "cloudResourceHistory", "pvePublicIp"} {
		if err := db.C(name).Ensure(ctx); err != nil {
			return false, err
		}
	}
	removed := false
	release := func() error {
		return db.WithTx(ctx, func(tc context.Context) error {
			if err := db.LockXact(tc, allocationLock(expected.ServiceID, expected.ServerID, expected.NIC)); err != nil {
				return err
			}
			if err := db.LockXact(tc, "cloudResource:"+expected.ServiceID+":"+expected.ServerID); err != nil {
				return err
			}
			var server cloud.CloudResource
			_, present, err := db.C("cloudResource").FindOneForUpdate(tc, pgdoc.M{"serviceId": expected.ServiceID, "externalId": expected.ServerID}, &server)
			if err != nil || present {
				return err
			}
			var history cloud.History
			ok, err := db.C("cloudResourceHistory").FindOne(tc, pgdoc.M{"cloudResourceId": expectedServer.ID}, &history)
			if err != nil || !ok {
				return err
			}
			if history.ServiceID != expectedServer.ServiceID || history.ProjectID != expectedServer.ProjectID || history.ExternalID != expectedServer.ExternalID || history.Type != cloud.TypeServer || history.SourceSnapshotHash == "" || history.SourceSnapshotHash != cloud.InventorySnapshotHash(expectedServer) || history.Region != expectedServer.Region || !sameIPProofJSON(history.CreatedAt, expectedServer.CreatedAt) || !sameIPProofJSON(history.Data, expectedServer.Data) {
				return nil
			}
			var current PublicIP
			ok, err = r.col.GetForUpdate(tc, expected.ID, &current)
			if err != nil || !ok {
				return err
			}
			if !sameIPProofJSON(current, expected) {
				return nil
			}
			removed, err = r.col.DeleteByID(tc, expected.ID)
			return err
		})
	}
	if r.guard != nil && expected.IsPublic() {
		if err := r.guard(ctx, expected.ProjectID, []PublicIP{expected}, release); err != nil {
			return false, err
		}
	} else if err := release(); err != nil {
		return false, err
	}
	return removed, nil
}
func sameIPProofJSON(a, b any) bool {
	x, e := json.Marshal(a)
	if e != nil {
		return false
	}
	y, e := json.Marshal(b)
	return e == nil && bytes.Equal(x, y)
}

// allocationLock is shared by allocation and inventory release. Exact-ID JSON
// CAS prevents a freed/reallocated IP from being deleted by an older sample;
// locking the VMID/NIC prevents bulk NIC release semantics from creeping back in.
func allocationLock(service, server, nic string) string {
	return fmt.Sprintf("pve-ip:%s:%s:%s", service, server, nic)
}
