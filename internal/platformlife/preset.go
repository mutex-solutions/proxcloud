package platformlife

// preset.go — the install presets (v0.9.0 design, package P1): a named start for the settings. A preset fills what a
// spec leaves out and is not stored: the saved spec names every machine, count and size itself, and an apply never
// asks which preset it came from. web/admin/src/pages/platform/model.ts holds the same figures for the form.
//
// When the sandbox service ships, a preset is also where its placement gets its default (Spec.Sandbox): homelab
// shared, standard separate. Nothing sets it today.

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
var presets = map[string]struct {
	seed, controlPlane, group presetSize
	replicas, perGroup        int
}{
	PresetHomelab:  {presetSize{minCores, minMemMiB, 30}, presetSize{minCores, minMemMiB, 30}, presetSize{minCores, minMemMiB, 40}, 1, 1},
	PresetStandard: {presetSize{2, 6144, 30}, presetSize{2, 4096, 30}, presetSize{4, 8192, 40}, 3, 2},
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
	if n := &s.Network; preset == PresetHomelab && oneNode && n.Mode == "" && n.Bridge == "" && n.VLAN == 0 {
		n.Mode, n.Bridge = NetworkRouted, "pcplat0"
	}
	return nil
}
