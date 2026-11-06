package metrics

// pve.go — the Proxmox traffic source. PVE keeps per-tap byte counters only in the running QEMU's
// view (status/current `nics`): no history, and they restart from 0 whenever the tap is recreated
// (VM stop/start, migration). So this fetcher turns a sample into month-to-date totals itself, in
// the nicCounter collection, and the job samples every minute to keep what a reset can lose small.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

const (
	metricIn  = "network.incoming.bytes"
	metricOut = "network.outgoing.bytes"
)

// NICCounter is one public NIC's traffic state (collection nicCounter, id = <row id>/<public IP
// row's externalId>): last raw counters plus the current month's accumulated bytes. Keying on the
// SERVER row id keeps a reused vmid's traffic apart; keying on the IP row keeps a re-plugged NIC,
// whose tap restarts from 0, apart from the previous one.
type NICCounter struct {
	ID         string `json:"id,omitempty"`
	ResourceID string `json:"resourceId,omitempty"`
	NIC        string `json:"nic,omitempty"`
	Month      string `json:"month,omitempty"`
	PrevIn     int64  `json:"prevIn"`
	PrevOut    int64  `json:"prevOut"`
	InBytes    int64  `json:"inBytes"`
	OutBytes   int64  `json:"outBytes"`
}

// counterDelta is the bytes since the last sample. A counter that went backwards was recreated, so
// everything it shows is new (what ran between the last sample and the reset is lost).
func counterDelta(prev, cur int64) int64 {
	if cur >= prev {
		return cur - prev
	}
	return cur
}

// apply folds one raw sample in. The first sample of a NIC counts in full: a public NIC is plugged
// by the portal, so its tap started at 0 with the allocation this counter is keyed on. A new month
// restarts the totals, keeping the raw counters so nothing from the old month carries over.
func (c *NICCounter) apply(month string, in, out int64) {
	if c.Month != month {
		c.Month, c.InBytes, c.OutBytes = month, 0, 0
	}
	c.InBytes += counterDelta(c.PrevIn, in)
	c.OutBytes += counterDelta(c.PrevOut, out)
	c.PrevIn, c.PrevOut = in, out
}

// PVEFetcher is the MeasureFetcher of a Proxmox provider. One lives for one job pass: it reads the
// VM list once. Only the public NICs are measured (plan decision 12: SDN egress is not billed).
type PVEFetcher struct {
	svc      *pve.Service
	cloud    *cloud.Repo
	counters *pgdoc.Store
	now      func() time.Time
	vms      map[string]pve.ClusterResource
}

func NewPVEFetcher(svc *pve.Service, cloudRepo *cloud.Repo, db *pgdoc.DB, now func() time.Time) *PVEFetcher {
	return &PVEFetcher{svc: svc, cloud: cloudRepo, counters: db.C("nicCounter"), now: now}
}

// SearchInstanceInterfaces samples the VM's public NICs and returns every counter of the row, the
// NICs since detached included, so their month-to-date stays in the aggregate.
func (f *PVEFetcher) SearchInstanceInterfaces(ctx context.Context, vmid string) ([]Resource, error) {
	row, err := f.cloud.FindByServiceIDAndExternalID(ctx, f.svc.ServiceID, vmid)
	if err != nil || row == nil || row.Type != cloud.TypeServer {
		return nil, err
	}
	if err := f.sample(ctx, row); err != nil {
		return nil, err
	}
	var all []NICCounter
	if err := f.counters.Find(ctx, pgdoc.M{"resourceId": row.ID}, &all); err != nil {
		return nil, err
	}
	public := true
	out := make([]Resource, 0, len(all))
	for _, c := range all {
		out = append(out, Resource{
			ID: c.ID, Name: c.NIC, Public: &public,
			Metrics: map[string]string{metricIn: c.ID + "#in", metricOut: c.ID + "#out"},
		})
	}
	return out, nil
}

// sample folds the running VM's counters in (a stopped VM has none: nothing moved).
func (f *PVEFetcher) sample(ctx context.Context, row *cloud.CloudResource) error {
	fips, err := f.cloud.FindByServiceProjectAndType(ctx, f.svc.ServiceID, row.ProjectID, cloud.TypeFloatingIP)
	if err != nil {
		return err
	}
	var st *pve.VMStatus
	month := f.now().UTC().Format("2006-01")
	for i := range fips {
		fip, _ := fips[i].Data["floatingIp"].(map[string]any)
		nic, _ := fip["nic"].(string)
		if server, _ := fip["server_id"].(string); server != row.ExternalID || nic == "" {
			continue
		}
		if st == nil {
			vm, err := f.vm(ctx, row.ExternalID)
			if err != nil {
				return err
			}
			if vm == nil || vm.Status != "running" {
				return nil
			}
			if st, err = f.svc.C.GetVMStatus(ctx, vm.Node, vm.VMID); err != nil {
				return err
			}
		}
		tap, sample, ok := tapSample(row.ExternalID, nic, st.NICs)
		if !ok {
			continue
		}
		id := row.ID + "/" + fips[i].ExternalID
		c := NICCounter{ID: id, ResourceID: row.ID, NIC: tap}
		if _, err := f.counters.Get(ctx, id, &c); err != nil {
			return err
		}
		c.apply(month, sample.NetIn, sample.NetOut)
		if err := f.counters.Upsert(ctx, id, &c); err != nil {
			return err
		}
	}
	return nil
}

// tapSample maps a config NIC ("net1") to its tap ("tap<vmid>i1") and that tap's counters.
func tapSample(vmid, nic string, nics map[string]pve.NICCounters) (string, pve.NICCounters, bool) {
	idx, ok := strings.CutPrefix(nic, "net")
	if !ok {
		return "", pve.NICCounters{}, false
	}
	tap := "tap" + vmid + "i" + idx
	s, ok := nics[tap]
	return tap, s, ok
}

func (f *PVEFetcher) vm(ctx context.Context, vmid string) (*pve.ClusterResource, error) {
	if f.vms == nil {
		page, err := f.svc.C.ClusterResources(ctx, "vm")
		if err != nil {
			return nil, err
		}
		f.vms = make(map[string]pve.ClusterResource, len(page))
		for _, r := range page {
			f.vms[strconv.Itoa(r.VMID)] = r
		}
	}
	if r, ok := f.vms[vmid]; ok {
		return &r, nil
	}
	return nil, nil
}

// MeasuresMBForCurrentMonth reads a counter's accumulated total, 0 when it last moved in another
// month.
func (f *PVEFetcher) MeasuresMBForCurrentMonth(ctx context.Context, metricID string, start time.Time) (decimal.Decimal, error) {
	id, dir, ok := strings.Cut(metricID, "#")
	if !ok {
		return decimal.Zero, fmt.Errorf("pve metrics: bad metric id %q", metricID)
	}
	var c NICCounter
	found, err := f.counters.Get(ctx, id, &c)
	if err != nil || !found || c.Month != start.UTC().Format("2006-01") {
		return decimal.Zero, err
	}
	bytes := c.InBytes
	if dir == "out" {
		bytes = c.OutBytes
	}
	return divDecimal64(decimal.NewFromInt(bytes), decimal.NewFromInt(bytesPerMB))
}
