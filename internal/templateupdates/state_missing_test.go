package templateupdates

import (
	"context"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

// A template that is gone leaves its receipt as a row of the list. Proxmox gives the freed VMID to the next guest,
// often the next import on that node: the old row then names no VMID, so the list does not show one VMID twice. While
// nothing holds the VMID the row keeps it.
func TestMissingTemplateRowDropsAVMIDAnotherGuestHolds(t *testing.T) {
	old := Receipt{ID: "old", Step: StepSucceeded, VMID: 9201, Payload: VMPayload{ServiceID: "svc", Node: "pve1", Digest: "sha256:" + strings.Repeat("a", 64)}}
	marked := old
	marked.TemplateGone = true
	taken := map[string]ProviderRows{"svc": {Rows: []pve.ClusterResource{{Type: "qemu", VMID: 9201, Node: "pve1", Template: 1, Pool: pve.TemplatePool, Name: "another-image"}}}}
	free := map[string]ProviderRows{"svc": {Rows: []pve.ClusterResource{{Type: "qemu", VMID: 100, Node: "pve1"}}}}
	s := &Service{}
	for name, rec := range map[string]Receipt{"read as missing": old, "marked gone": marked} {
		if st := s.stateOf(context.Background(), rec, taken, nil); st.State != StateTemplateMissing || st.VMID != 0 || len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "9201") {
			t.Errorf("%s, the VMID taken: %+v", name, st)
		}
		if st := s.stateOf(context.Background(), rec, free, nil); st.State != StateTemplateMissing || st.VMID != 9201 || len(st.Warnings) != 0 {
			t.Errorf("%s, the VMID free: %+v", name, st)
		}
	}
}
