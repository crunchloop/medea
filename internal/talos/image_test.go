package talos

import "testing"

func TestDeriveInstallerImage(t *testing.T) {
	cases := []struct {
		name, current, target, want string
	}{
		{
			name:    "factory schematic preserved",
			current: "factory.talos.dev/installer/abc123def:v1.13.5",
			target:  "v1.13.6",
			want:    "factory.talos.dev/installer/abc123def:v1.13.6",
		},
		{
			name:    "plain ghcr installer",
			current: "ghcr.io/siderolabs/installer:v1.13.5",
			target:  "v1.13.6",
			want:    "ghcr.io/siderolabs/installer:v1.13.6",
		},
		{
			name:    "empty falls back to default",
			current: "",
			target:  "v1.13.6",
			want:    "ghcr.io/siderolabs/installer:v1.13.6",
		},
		{
			name:    "no tag gets one",
			current: "ghcr.io/siderolabs/installer",
			target:  "v1.13.6",
			want:    "ghcr.io/siderolabs/installer:v1.13.6",
		},
		{
			name:    "registry port not mistaken for tag",
			current: "registry.local:5000/installer/abc123",
			target:  "v1.13.6",
			want:    "registry.local:5000/installer/abc123:v1.13.6",
		},
	}
	for _, tc := range cases {
		if got := DeriveInstallerImage(tc.current, tc.target); got != tc.want {
			t.Errorf("%s: DeriveInstallerImage(%q,%q) = %q, want %q", tc.name, tc.current, tc.target, got, tc.want)
		}
	}
}
