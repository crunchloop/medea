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
