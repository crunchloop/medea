//go:build integration

package itest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bilby91/medea/internal/kube"
	"github.com/bilby91/medea/internal/talos"
)

// TestCaptureSecrets validates the provisioning secrets-capture path
// (provisioning-plane.md §5): secrets.NewBundleFromConfig over a live
// control-plane machine config must yield a usable secrets.yaml (the bundle a
// joining node's config is minted from). Reuses the docker scratch cluster.
func TestCaptureSecrets(t *testing.T) {
	c := Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	kc, err := kube.New(c.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := kc.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cpIP string
	for _, n := range nodes {
		if n.Role == "controlplane" {
			cpIP = n.InternalIP
		}
	}
	if cpIP == "" {
		t.Fatal("no control-plane node")
	}

	tc, err := talos.New(ctx, c.Talosconfig, []string{cpIP})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()

	bundle, err := tc.CaptureSecrets(ctx, cpIP)
	if err != nil {
		t.Fatalf("CaptureSecrets: %v", err)
	}
	// A real secrets bundle has the cluster section, tokens, trustd info, and CAs.
	s := strings.ToLower(string(bundle))
	for _, want := range []string{"cluster", "secrets", "trustdinfo", "certs"} {
		if !strings.Contains(s, want) {
			t.Fatalf("secrets bundle missing %q:\n%s", want, string(bundle))
		}
	}
}
