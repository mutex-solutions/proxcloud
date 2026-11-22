package platformlife

// preset.go — the install presets (v0.9.0 design, package P1): a named start for the settings. A preset fills what a
// spec leaves out and is not stored: the saved spec names every machine, count and size itself, and an apply never
// asks which preset it came from. web/admin/src/pages/platform/model.ts holds the same figures for the form.
//
// A preset is also where a sandbox block the spec holds gets its defaults (Spec.Sandbox): homelab shared with one
// machine of the smallest size, standard separate with two machines and a system machine. A preset never adds the
// block: sandboxes are off until the operator asks for them.

import "fmt"

const (
	PresetHomelab  = "homelab"
	PresetStandard = "standard"
	PresetCustom   = "custom"
)

type presetSize struct{ cores, memoryMiB, diskGiB int }

// A preset's figures. homelab is the smallest machine Validate takes (minCores, minMemMiB), one control plane machine
// and one machine per group and node: what the lab runs. standard is three control plane machines and at least two
// machines per group, with the sizes the form has always started from.
//
// Sandboxes (measured on the spike of 2026-11-21: a node's own pods ask for about one core, an idle sandbox takes 35 to
// 50 MiB): homelab's one machine of 2 cores and 4 GiB holds ONE sandbox of one vCPU, which is what a lab of 5 GiB nodes
// has room for; a homelab that wants several names 4 cores or more itself. standard's two machines of 4 cores hold
// about three each, and its system machine runs sandbox-proxy and the sandbox controller.
var presets = map[string]struct {
	seed, controlPlane, group presetSize
	replicas, perGroup        int
	sandboxPlacement          string
	sandbox, sandboxSystem    presetSize
	sandboxCount              int
}{
	PresetHomelab: {presetSize{minCores, minMemMiB, 30}, presetSize{minCores, minMemMiB, 30}, presetSize{minCores, minMemMiB, 40}, 1, 1,
		SandboxShared, presetSize{minCores, minMemMiB, 40}, presetSize{}, 1},
	PresetStandard: {presetSize{2, 6144, 30}, presetSize{2, 4096, 30}, presetSize{4, 8192, 40}, 3, 2,
		SandboxSeparate, presetSize{4, 8192, 40}, presetSize{minCores, minMemMiB, 30}, 2},
}

func (z presetSize) fill(cores, mem, disk *int) {
	if *cores == 0 {
		*cores = z.cores
	}
	if *mem == 0 {
		*mem = z.memoryMiB
	}
	if *disk == 0 {
		*disk = z.diskGiB
	}
}

// ApplyPreset fills what s leaves out with the preset's values; a value s gives always stays. "" and custom fill
// nothing. three says whether the installation's release builds three control plane machines (Installation.BuildsThree):
// where it does not, standard fills one, as the console's form does. A node nobody names is the seed's: the control plane's, then each group's. Homelab also makes the platform
// network one the node routes (mode routed, vnet pcplat0) when the spec names no mode, no bridge and no tag and every
// machine is on one node: on one node an existing bridge behind a router breaks the path from tenants to the platform
// (docs/managed-services.md, "The path from tenants to the platform").
func ApplyPreset(preset string, s *Spec, three bool) error {
	if preset == "" || preset == PresetCustom {
		return nil
	}
	p, ok := presets[preset]
	if !ok {
		return fmt.Errorf("preset %q is not %s, %s or %s", preset, PresetHomelab, PresetStandard, PresetCustom)
	}
	p.seed.fill(&s.Seed.Cores, &s.Seed.MemoryMiB, &s.Seed.DiskGiB)
	if len(s.Clusters) == 0 {
		s.Clusters = []ClusterSpec{{}}
	}
	oneNode := true
	for i := range s.Clusters {
		c := &s.Clusters[i]
		cp := &c.ControlPlane
		if cp.Node == "" {
			cp.Node = s.Seed.Node
		}
		if cp.Replicas == 0 {
			cp.Replicas = p.replicas
			if !three {
				cp.Replicas = 1
			}
		}
		p.controlPlane.fill(&cp.Cores, &cp.MemoryMiB, &cp.DiskGiB)
		oneNode = oneNode && cp.Node == s.Seed.Node
		for _, name := range NodeGroupNames {
			gi := -1
			for j := range c.NodeGroups {
				if c.NodeGroups[j].Name == name {
					gi = j
				}
			}
			if gi < 0 {
				c.NodeGroups = append(c.NodeGroups, NodeGroup{Name: name})
				gi = len(c.NodeGroups) - 1
			}
			g := &c.NodeGroups[gi]
			if len(g.Nodes) == 0 {
				g.Nodes = []string{cp.Node}
			}
			if g.Count == 0 {
				g.Count = max(p.perGroup, len(g.Nodes))
			}
			p.group.fill(&g.Cores, &g.MemoryMiB, &g.DiskGiB)
			oneNode = oneNode && len(g.Nodes) == 1 && g.Nodes[0] == s.Seed.Node
		}
	}
	if sb := s.Sandbox; sb != nil && sb.Placement != SandboxOff {
		if sb.Placement == "" {
			sb.Placement = p.sandboxPlacement
		}
		node := s.Clusters[0].ControlPlane.Node
		fill := func(m *SandboxMachines, z presetSize, count int) {
			if len(m.Nodes) == 0 {
				m.Nodes = []string{node}
			}
			if m.Count == 0 {
				m.Count = max(count, len(m.Nodes))
			}
			z.fill(&m.Cores, &m.MemoryMiB, &m.DiskGiB)
			oneNode = oneNode && len(m.Nodes) == 1 && m.Nodes[0] == s.Seed.Node
		}
		fill(&sb.SandboxMachines, p.sandbox, p.sandboxCount)
		if sb.Placement == SandboxSeparate {
			if sb.System == nil {
				sb.System = &SandboxMachines{}
			}
			z := p.sandboxSystem
			if z == (presetSize{}) { // homelab asked for separate: the smallest machine
				z = presetSize{minCores, minMemMiB, 30}
			}
			fill(sb.System, z, 1)
		}
	}
	if n := &s.Network; preset == PresetHomelab && oneNode && n.Mode == "" && n.Bridge == "" && n.VLAN == 0 {
		n.Mode, n.Bridge = NetworkRouted, "pcplat0"
	}
	return nil
}
