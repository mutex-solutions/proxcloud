package pve

import (
	"reflect"
	"testing"
)

func TestDiscoverTemplates(t *testing.T) {
	const pub = "proxcloud;pc-public;pc-img-debian-13"
	row := func(vmid int, node, pool, tags string) ClusterResource {
		return ClusterResource{Type: "qemu", VMID: vmid, Node: node, Pool: pool, Tags: tags, Template: 1, MaxDisk: 3<<30 + 1}
	}
	notTemplate := row(120, "pve1", TemplatePool, pub+";pc-v-202611070000")
	notTemplate.Template = 0
	got := DiscoverTemplates([]ClusterResource{
		row(9000, "pve1", TemplatePool, pub), // no version: ranks oldest on pve1
		row(9101, "pve1", TemplatePool, pub+";pc-v-202611010000"),
		row(9102, "pve1", TemplatePool, pub+";pc-v-202611070000"),
		row(9103, "pve2", TemplatePool, pub+";pc-v-202611010000"), // pve2 lags behind but stays usable
		row(9200, "pve1", TemplatePool, "proxcloud;pc-public;pc-img-ubuntu-24.04"),
		// None of these may enter the catalog.
		notTemplate,
		row(9300, "pve1", "pc-tenant", pub+";pc-v-209901010000"),                        // tenant pool
		row(9301, "pve1", TemplatePool, "proxcloud;pc-img-debian-13;pc-v-209901010000"), // not public
		row(9302, "pve1", TemplatePool, "proxcloud;pc-public"),                          // no slug
	})
	want := []DiscoveredTemplate{
		{Slug: "debian-13", Node: "pve1", VMID: 9102, Copies: map[string]TemplateCopy{
			"pve1": {VMID: 9102, Version: "202611070000", DiskGb: 4},
			"pve2": {VMID: 9103, Version: "202611010000", DiskGb: 4},
		}},
		{Slug: "ubuntu-24.04", Node: "pve1", VMID: 9200, Copies: map[string]TemplateCopy{
			"pve1": {VMID: 9200, DiskGb: 4},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
