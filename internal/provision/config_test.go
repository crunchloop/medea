package provision

import (
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
)

func testBundle(t *testing.T) *secrets.Bundle {
	t.Helper()
	b, err := secrets.NewBundle(secrets.NewClock(), config.TalosVersionCurrent)
	if err != nil {
		t.Fatalf("new bundle: %v", err)
	}
	return b
}

func TestRenderWorkerConfig(t *testing.T) {
	out, err := RenderWorkerConfig(WorkerConfigInput{
		ClusterName:          "home",
		ControlPlaneEndpoint: "https://10.5.0.2:6443",
		KubernetesVersion:    "v1.36.2",
		InstallDisk:          "/dev/sda",
		InstallImage:         "factory.talos.dev/metal-installer/abc:v1.13.5",
		Secrets:              testBundle(t),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	// It's a worker config that joins the given cluster on the given endpoint,
	// using the install image/disk we asked for.
	for _, want := range []string{"type: worker", "10.5.0.2:6443", "/dev/sda", "factory.talos.dev/metal-installer/abc:v1.13.5"} {
		if !strings.Contains(s, want) {
			t.Fatalf("worker config missing %q:\n%s", want, s)
		}
	}
	// It must NOT be a control-plane config.
	if strings.Contains(s, "type: controlplane") {
		t.Fatalf("rendered a control-plane config:\n%s", s)
	}
}

func TestRenderWorkerConfigRequiresSecretsAndEndpoint(t *testing.T) {
	if _, err := RenderWorkerConfig(WorkerConfigInput{ClusterName: "home", ControlPlaneEndpoint: "https://x:6443"}); err == nil {
		t.Fatal("expected error without a secrets bundle")
	}
	if _, err := RenderWorkerConfig(WorkerConfigInput{Secrets: testBundle(t)}); err == nil {
		t.Fatal("expected error without cluster name / endpoint")
	}
}

func TestGenerateSecretsMintsFresh(t *testing.T) {
	a, err := GenerateSecrets()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if a.Cluster == nil || a.Cluster.ID == "" {
		t.Fatal("generated bundle has no cluster id")
	}
	b, err := GenerateSecrets()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Each call must MINT a distinct cluster (fresh PKI) — the inverse of capture.
	if a.Cluster.ID == b.Cluster.ID {
		t.Fatal("two generations share a cluster id — not minting fresh PKI")
	}
}

func TestRenderControlPlaneConfig(t *testing.T) {
	out, err := RenderControlPlaneConfig(ControlPlaneConfigInput{
		ClusterName:                    "home",
		ControlPlaneEndpoint:           "https://192.168.14.160:6443",
		KubernetesVersion:              "v1.36.1",
		InstallDisk:                    "/dev/nvme0n1",
		InstallImage:                   "factory.talos.dev/metal-installer/xyz:v1.13.5",
		Secrets:                        testBundle(t),
		AllowSchedulingOnControlPlanes: true,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		"type: controlplane",
		"192.168.14.160:6443",
		"/dev/nvme0n1",
		"factory.talos.dev/metal-installer/xyz:v1.13.5",
		"allowSchedulingOnControlPlanes: true",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("control-plane config missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "type: worker") {
		t.Fatalf("rendered a worker config:\n%s", s)
	}
}

func TestRenderControlPlaneConfigAppliesPatches(t *testing.T) {
	// A strategic-merge patch like talos/patches/controlplane.yaml (CNI none).
	patch := []byte("cluster:\n  network:\n    cni:\n      name: none\n")
	out, err := RenderControlPlaneConfig(ControlPlaneConfigInput{
		ClusterName:          "home",
		ControlPlaneEndpoint: "https://10.0.0.1:6443",
		KubernetesVersion:    "v1.36.1",
		Secrets:              testBundle(t),
		Patches:              [][]byte{patch},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(out), "name: none") {
		t.Fatalf("patch not applied (expected cni name: none):\n%s", out)
	}
}

func TestRenderControlPlaneConfigCNIOption(t *testing.T) {
	// The typed bring-your-own-CNI intent: cni=none + kube-proxy off. Medea builds
	// the patch itself (the retired talos/patches/controlplane.yaml settings), so
	// the caller passes no patch for these.
	out, err := RenderControlPlaneConfig(ControlPlaneConfigInput{
		ClusterName:          "home",
		ControlPlaneEndpoint: "https://10.0.0.1:6443",
		KubernetesVersion:    "v1.36.1",
		Secrets:              testBundle(t),
		CNI:                  "none",
		DisableKubeProxy:     true,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	for _, want := range []string{"name: none", "disabled: true"} {
		if !strings.Contains(s, want) {
			t.Fatalf("CNI option not applied (expected %q):\n%s", want, s)
		}
	}
}

func TestRenderControlPlaneConfigNoCNIByDefault(t *testing.T) {
	// Empty CNI + kube-proxy left on: Medea injects no CNI patch (Talos default).
	out, err := RenderControlPlaneConfig(ControlPlaneConfigInput{
		ClusterName:          "home",
		ControlPlaneEndpoint: "https://10.0.0.1:6443",
		KubernetesVersion:    "v1.36.1",
		Secrets:              testBundle(t),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The default Talos config carries a commented-out cni example, so assert on the
	// active override instead: Medea must not have injected `name: none`.
	if strings.Contains(string(out), "name: none") {
		t.Fatalf("expected no cni override with default CNI:\n%s", out)
	}
}

func TestCNIProxyPatch(t *testing.T) {
	if p := cniProxyPatch("", false); p != nil {
		t.Fatalf("expected nil patch when nothing requested, got %q", p)
	}
	p := string(cniProxyPatch("none", true))
	for _, want := range []string{"cluster:", "cni:", "name: none", "proxy:", "disabled: true"} {
		if !strings.Contains(p, want) {
			t.Fatalf("patch missing %q:\n%s", want, p)
		}
	}
	// CNI only — no proxy stanza.
	if got := string(cniProxyPatch("cilium", false)); strings.Contains(got, "proxy:") {
		t.Fatalf("did not expect proxy stanza:\n%s", got)
	}
}

func TestRenderControlPlaneConfigRequiresSecretsAndEndpoint(t *testing.T) {
	if _, err := RenderControlPlaneConfig(ControlPlaneConfigInput{ClusterName: "home", ControlPlaneEndpoint: "https://x:6443"}); err == nil {
		t.Fatal("expected error without a secrets bundle")
	}
	if _, err := RenderControlPlaneConfig(ControlPlaneConfigInput{Secrets: testBundle(t)}); err == nil {
		t.Fatal("expected error without cluster name / endpoint")
	}
}
