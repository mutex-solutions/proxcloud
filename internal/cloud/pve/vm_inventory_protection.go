package pve

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// Inventory mapping is GET-only: StampProtection also heals tags/HA, so it must
// not run from List before the whole cache scope has passed its visibility proof.
func (s *Service) inventoryProtection(ctx context.Context, r ClusterResource, cfg VMConfig) (map[string]any, error) {
	var jobs []ReplicationJob
	err := s.C.inventoryRead(ctx, "/cluster/replication", nil, &jobs)
	if err != nil || jobs == nil {
		return nil, errors.Join(err, errors.New("protection inventory unknown"))
	}
	for _, j := range jobs {
		if j.ID == "" || j.Guest <= 0 || j.Target == "" {
			return nil, errors.New("replication identity unknown")
		}
	}
	var job *ReplicationJob
	for i := range jobs {
		if jobs[i].Guest == r.VMID && jobs[i].RemoveJob == "" {
			job = &jobs[i]
		}
	}
	if job == nil && !HasTag(r.Tags, tagProtected) {
		return nil, nil
	}
	var ha []HAResource
	err = s.C.inventoryRead(ctx, "/cluster/ha/resources", nil, &ha)
	if err != nil || ha == nil {
		return nil, errors.Join(err, errors.New("protection inventory unknown"))
	}
	for _, h := range ha {
		if h.SID == "" || h.State == "" {
			return nil, errors.New("HA identity unknown")
		}
	}
	managed := false
	for _, h := range ha {
		managed = managed || h.SID == "vm:"+strconv.Itoa(r.VMID)
	}
	var pr map[string]any
	if job == nil {
		pr = map[string]any{"state": ProtectionSyncing, "mode": ProtectionModeShared, "target": "", "schedule": "", "rpoTargetMinutes": 0, "lastSync": 0, "failCount": 0, "error": "", "ha": managed}
		if managed {
			pr["state"] = ProtectionActive
		}
	} else {
		var status map[string]json.RawMessage
		err := s.C.inventoryRead(ctx, "/nodes/"+r.Node+"/replication/"+job.ID+"/status", nil, &status)
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"last_sync", "fail_count"} {
			var n int64
			if raw, ok := status[key]; !ok || string(raw) == "null" || json.Unmarshal(raw, &n) != nil || n < 0 {
				return nil, errors.New("replication status incomplete")
			}
		}
		raw, _ := json.Marshal(status)
		var st ReplicationStatus
		err = json.Unmarshal(raw, &st)
		if err != nil {
			return nil, err
		}
		state := ProtectionSyncing
		if managed {
			state = ProtectionActive
			if st.FailCount > 0 || time.Now().Unix()-st.LastSync > 2*replicationScheduleMinutes*60 {
				state = ProtectionDegraded
			}
		}
		pr = map[string]any{"state": state, "target": job.Target, "schedule": job.Schedule, "rpoTargetMinutes": replicationScheduleMinutes, "lastSync": st.LastSync, "failCount": st.FailCount, "error": st.Error, "ha": managed}
	}
	pr["diskGb"] = diskGbOf(cfg)
	if since, err := strconv.ParseInt(TagValue(cfg.Str("tags"), tagProtectedSince), 10, 64); err == nil {
		pr["since"] = time.Unix(since, 0).UTC().Format(time.RFC3339)
	}
	return pr, nil
}

// ReconcileVerifiedProtection retains the existing automatic HA healing, but
// only after every project provider in this invocation succeeded. Recheck the
// same owned/current snapshot immediately before calling the existing healer.
func (p *ServerSyncProvider) ReconcileVerifiedProtection(ctx context.Context) error {
	if p.inventory == nil || !p.inventory.verified {
		return vmInventoryError(p.Type(), "", "protection heal requires verified inventory", nil)
	}
	if err := p.inventory.verify(ctx, p.Type(), nil, nil); err != nil {
		return err
	}
	for _, r := range p.inventory.page {
		if r.Type != "qemu" || r.Pool != p.inventory.pool.PoolID || r.Template == 1 || isHolder(r.Tags) {
			continue
		}
		if _, err := p.svc.ReconcileProtection(ctx, r); err != nil {
			return err
		}
	}
	return nil
}
