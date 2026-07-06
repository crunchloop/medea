//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/crunchloop/medea/internal/kube"
	"github.com/crunchloop/medea/internal/talos/k8supgrade"
)

// k8s upgrade path versions: boot the scratch cluster one patch below the
// release default and upgrade up to it. Both are real published k8s patches and
// "1.36->1.36" is a supported Talos upgrade path. The Talos image is pinned to
// match the imported main-module version (machinery + k8supgrade are v1.13.5).
const (
	k8sFrom    = "v1.36.1"
	k8sTo      = "v1.36.2"
	talosImage = "ghcr.io/siderolabs/talos:v1.13.5"
)

// TestK8sUpgrade validates the Kubernetes upgrade path (upgrade-k8s) against a
// scratch docker Talos cluster — the tier the design earmarks for it
// (design/talos-client.md §9, PRD §9.2). Unlike UpgradeOS (an A/B image swap +
// reboot, which needs QEMU/hardware — see TestQemuUpgrade), upgrade-k8s rewrites
// control-plane manifests and rolls kubelets with no node reboot, so the docker
// provisioner exercises the real orchestration faithfully. This is also where
// the version-coupled k8supgrade main-module import is validated.
func TestK8sUpgrade(t *testing.T) {
	c := StartWith(t, Options{K8sVersion: k8sFrom, TalosImage: talosImage})
	ctx := context.Background()

	kc, err := kube.New(c.Kubeconfig)
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}

	nodes, err := kc.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("no nodes in scratch cluster")
	}

	// Sanity: the cluster booted at the expected lower patch.
	for _, n := range nodes {
		if n.KubeletVersion != k8sFrom {
			t.Fatalf("node %s booted at %s, expected %s", n.Name, n.KubeletVersion, k8sFrom)
		}
	}

	// Build the quarantined upgrader from the scratch cluster's creds + the
	// control-plane node IP (the harness helper used elsewhere).
	endpoints := []string{controlPlaneNodeIP(t, c.Name)}
	up, err := k8supgrade.New(c.Talosconfig, endpoints)
	if err != nil {
		t.Fatalf("k8supgrade.New: %v", err)
	}

	// Trigger the Talos-orchestrated upgrade (blocks until Talos reports done).
	t.Logf("upgrading kubernetes %s -> %s", k8sFrom, k8sTo)
	if err := up.UpgradeK8s(ctx, k8sFrom, k8sTo); err != nil {
		t.Fatalf("UpgradeK8s(%s -> %s): %v", k8sFrom, k8sTo, err)
	}

	// Monitor-to-completion: Talos drives the upgrade; we observe convergence by
	// polling kubelet versions until every node reports the target. Transient
	// control-plane blips during the control-plane component roll are treated as
	// not-yet-converged (park-and-retry), like the OS path's waitHealthy.
	deadline := time.Now().Add(8 * time.Minute)
	for {
		converged := true
		for _, n := range nodes {
			v, err := kc.KubeletVersion(ctx, n.Name)
			if err != nil || v != k8sTo {
				converged = false
				t.Logf("waiting: node %s at %q (err=%v), want %s", n.Name, v, err, k8sTo)
				break
			}
		}
		if converged {
			t.Logf("all %d nodes converged to %s", len(nodes), k8sTo)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nodes did not converge to %s before deadline", k8sTo)
		}
		time.Sleep(10 * time.Second)
	}
}
