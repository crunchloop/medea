//go:build integration

package itest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crunchloop/medea/internal/kube"
	"github.com/crunchloop/medea/internal/talos"
)

// TestWrappersAgainstRealCluster is the first integration test: it validates the
// talos/kube wrappers (which have no unit coverage — they wrap real clients)
// against a live scratch cluster, mirroring the seed read-path.
func TestWrappersAgainstRealCluster(t *testing.T) {
	c := Start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	kc, err := kube.New(c.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := kc.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) < 2 {
		t.Fatalf("expected >= 2 nodes, got %d", len(nodes))
	}

	var sawCP bool
	for _, n := range nodes {
		if n.Role == "controlplane" {
			sawCP = true
		}
		if n.InternalIP == "" {
			t.Fatalf("node %q has no internal IP", n.Name)
		}
		if !strings.HasPrefix(n.KubeletVersion, "v1.") {
			t.Fatalf("node %q kubelet version %q", n.Name, n.KubeletVersion)
		}
	}
	if !sawCP {
		t.Fatal("no control-plane node found")
	}

	// Talos wrapper: Version against each node (endpoints from talosconfig).
	tc, err := talos.New(ctx, c.Talosconfig, nil)
	if err != nil {
		t.Fatalf("talos.New: %v", err)
	}
	defer tc.Close()

	for _, n := range nodes {
		v, err := tc.Version(ctx, n.InternalIP)
		if err != nil {
			t.Fatalf("talos Version(%s): %v", n.InternalIP, err)
		}
		if !strings.HasPrefix(v, "v") {
			t.Fatalf("talos version for %s = %q", n.InternalIP, v)
		}
	}
}
