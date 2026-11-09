package sandbox

import "fmt"

// ProjectQuota is what one project's sandboxes may hold at once (decision D10): a number of live
// sandboxes and their vCPU, memory and GPUs together. The provider's per-sandbox limits (Limits)
// apply on top. The project handler resolves it (project quota, else provider default, else
// DefaultProjectQuota) and passes it on every create; CreateSandbox checks it against the live
// cluster and the namespace ResourceQuota carries it as the backstop.
type ProjectQuota struct {
	Sandboxes int `json:"sandboxes"`
	VCPUs     int `json:"sandboxVcpus"`
	RAMMb     int `json:"sandboxRamMb"`
	GPUs      int `json:"sandboxGpus"`
}

// DefaultProjectQuota applies when neither the project nor the provider sets a key.
var DefaultProjectQuota = ProjectQuota{Sandboxes: 5, VCPUs: 16, RAMMb: 32 * 1024, GPUs: 0}

// check refuses s when the project's live sandboxes plus s would pass a limit. s is validated
// (CPU, memory and GPU filled in).
func (q ProjectQuota) check(live []map[string]any, s *Spec) error {
	var cpu, memMb, gpus int
	for _, it := range live {
		d := sandboxData(it)
		cpu += intOf(d["cpu"])
		memMb += intOf(d["memoryGiB"]) * 1024
		gpus += gpuCount(it)
	}
	over := func(key string, used, want, limit int) error {
		if used+want > limit {
			return fmt.Errorf("%w: %s: %d in use, %d requested, limit %d", ErrQuotaExceeded, key, used, want, limit)
		}
		return nil
	}
	for _, err := range []error{
		over("sandboxes", len(live), 1, q.Sandboxes),
		over("sandboxVcpus", cpu, s.CPU, q.VCPUs),
		over("sandboxRamMb", memMb, s.MemoryGiB*1024, q.RAMMb),
	} {
		if err != nil {
			return err
		}
	}
	if s.GPUs() > 0 {
		return over("sandboxGpus", gpus, s.GPUs(), q.GPUs)
	}
	return nil
}
