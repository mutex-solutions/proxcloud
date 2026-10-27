package cloud

import "testing"

func TestServerIsVolumeBackedFromMarkerOrNovaImage(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want bool
	}{
		{name: "marker", data: map[string]any{"volumeBacked": true}, want: true},
		{name: "empty string image", data: map[string]any{"server": map[string]any{"image": ""}}, want: true},
		{name: "empty image object", data: map[string]any{"server": map[string]any{"image": map[string]any{}}}, want: true},
		{name: "glance image", data: map[string]any{"server": map[string]any{"image": map[string]any{"id": "image-1"}}}},
		{name: "unknown shape", data: map[string]any{"server": map[string]any{"status": "BUILD"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ServerIsVolumeBacked(test.data); got != test.want {
				t.Fatalf("ServerIsVolumeBacked() = %v, want %v", got, test.want)
			}
		})
	}
}
