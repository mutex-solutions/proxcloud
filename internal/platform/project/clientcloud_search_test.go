package project

import (
	"reflect"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func TestSearchValuesForResource(t *testing.T) {
	cases := []struct {
		name string
		cr   *cloud.CloudResource
		want map[string]any
	}{
		{
			name: "VOLUME emits type (volume_type)",
			cr:   &cloud.CloudResource{Type: cloud.TypeVolume, Data: map[string]any{"volume": map[string]any{"name": "vol1", "volume_type": "ssd1"}}},
			want: map[string]any{"name": "vol1", "type": "ssd1"},
		},
		{
			name: "PORT emits device_id/device_owner/mac_address",
			cr: &cloud.CloudResource{Type: cloud.TypePort, Data: map[string]any{"port": map[string]any{
				"name": "p1", "device_id": "dev-9", "device_owner": "compute:pve", "mac_address": "fa:16:3e:aa:bb:cc"}}},
			want: map[string]any{"name": "p1", "device_id": "dev-9", "device_owner": "compute:pve", "mac_address": "fa:16:3e:aa:bb:cc"},
		},
		{
			name: "SECURITY_GROUP emits description",
			cr:   &cloud.CloudResource{Type: cloud.TypeSecurityGroup, Data: map[string]any{"securityGroup": map[string]any{"name": "sg1", "description": "web tier"}}},
			want: map[string]any{"name": "sg1", "description": "web tier"},
		},
		{
			name: "IMAGE emits image_type/status",
			cr:   &cloud.CloudResource{Type: cloud.TypeImage, Data: map[string]any{"image": map[string]any{"name": "ubuntu", "image_type": "snapshot", "status": "active"}}},
			want: map[string]any{"name": "ubuntu", "image_type": "snapshot", "status": "active"},
		},
		{
			name: "FLOATING_IP name == floating_ip_address (camelCase data key)",
			cr:   &cloud.CloudResource{Type: cloud.TypeFloatingIP, Data: map[string]any{"floatingIp": map[string]any{"floating_ip_address": "10.0.0.42"}}},
			want: map[string]any{"name": "10.0.0.42"},
		},
		{
			name: "SERVER emits flavor + ipv4",
			cr: &cloud.CloudResource{Type: cloud.TypeServer, Data: map[string]any{"server": map[string]any{
				"name":   "vm1",
				"flavor": map[string]any{"name": "t3.small"},
				"addresses": map[string]any{"net": []any{
					map[string]any{"version": float64(4), "addr": "192.168.1.5"},
					map[string]any{"version": float64(6), "addr": "fe80::1"},
				}},
			}}},
			want: map[string]any{"name": "vm1", "flavor": "t3.small", "ipv4": []string{"192.168.1.5"}},
		},
		{
			name: "NETWORK name only (default branch)",
			cr:   &cloud.CloudResource{Type: cloud.TypeNetwork, Data: map[string]any{"network": map[string]any{"name": "pw-net"}}},
			want: map[string]any{"name": "pw-net"},
		},
	}
	for _, c := range cases {
		got := searchValuesForResource(c.cr)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, got, c.want)
		}
	}
}
