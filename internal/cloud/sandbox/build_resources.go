package sandbox

// BuildConfig is independent of runtime Limits. Safe defaults target existing 8-CPU/~32Gi nodes
// with headroom; larger builders require a measured dedicated pool, not runtime oversubscription.
type BuildConfig struct {
	CPU           int               `json:"cpu"`
	MemoryGiB     int               `json:"memoryGiB"`
	DiskGiB       int               `json:"diskGiB"`
	MaxCPU        int               `json:"maxCpu"`
	MaxMemoryGiB  int               `json:"maxMemoryGiB"`
	MaxDiskGiB    int               `json:"maxDiskGiB"`
	MaxConcurrent int               `json:"maxConcurrent"`
	NodeSelector  map[string]string `json:"nodeSelector,omitempty"`
}

func (c BuildConfig) withDefaults() BuildConfig {
	for _, f := range []struct {
		p        *int
		def, cap int
	}{{&c.MaxCPU, 7, 64}, {&c.MaxMemoryGiB, 24, 256}, {&c.MaxDiskGiB, 60, 1024}, {&c.MaxConcurrent, 2, 10}} {
		if *f.p == 0 {
			*f.p = f.def
		}
		*f.p = min(max(*f.p, 1), f.cap)
	}
	if c.CPU == 0 {
		c.CPU = 2
	}
	c.CPU = min(max(c.CPU, 1), c.MaxCPU)
	if c.MemoryGiB == 0 {
		c.MemoryGiB = 4
	}
	c.MemoryGiB = min(max(c.MemoryGiB, 1), c.MaxMemoryGiB)
	if c.DiskGiB == 0 {
		c.DiskGiB = 30
	}
	c.DiskGiB = min(max(c.DiskGiB, 1), c.MaxDiskGiB)
	return c
}

type BuildResources struct {
	CPU       int `json:"buildCpu"`
	MemoryGiB int `json:"buildMemoryGiB"`
	DiskGiB   int `json:"buildDiskGiB"`
}

func (c BuildConfig) Resources(r BuildResources) (BuildResources, error) {
	c = c.withDefaults()
	if r.CPU == 0 {
		r.CPU = c.CPU
	}
	if r.MemoryGiB == 0 {
		r.MemoryGiB = c.MemoryGiB
	}
	if r.DiskGiB == 0 {
		r.DiskGiB = c.DiskGiB
	}
	if r.CPU < 1 || r.CPU > c.MaxCPU || r.MemoryGiB < 1 || r.MemoryGiB > c.MaxMemoryGiB || r.DiskGiB < 1 || r.DiskGiB > c.MaxDiskGiB {
		return r, badBuild("buildCpu must be 1..%d, buildMemoryGiB 1..%d and buildDiskGiB 1..%d", c.MaxCPU, c.MaxMemoryGiB, c.MaxDiskGiB)
	}
	return r, nil
}
