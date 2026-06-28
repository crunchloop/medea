//go:build integration

package itest

import (
	"context"
	"testing"

	"github.com/bilby91/medea/internal/kube"
	"github.com/bilby91/medea/internal/talos/k8supgrade"
)

// TestK8sUpgrade validates the Kubernetes upgrade path (upgrade-k8s) against a
// scratch docker Talos cluster — the tier the design earmarks for it
// (design/talos-client.md §9, PRD §9.2). Unlike UpgradeOS (an A/B image swap +
// reboot, which needs QEMU/hardware — see TestQemuUpgrade), upgrade-k8s rewrites
// control-plane manifests and rolls kubelets with no node reboot, so the docker
// provisioner exercises the real orchestration faithfully. This is also where
// the version-coupled k8supgrade main-module import is validated.
//
// SKIPPED until the M3 upstream wiring lands: k8supgrade.UpgradeK8s currently
// returns ErrNotImplemented. The harness slot and the convergence assertions
// exist now so turning the test on is a small change once the impl is real.
// To enable: implement internal/talos/k8supgrade, pick a real `to` patch the
// pinned Talos release supports, and delete the t.Skip below.
func TestK8sUpgrade(t *testing.T) {
	t.Skip("k8s upgrade path not implemented yet (M3); see internal/talos/k8supgrade")

	c := Start(t)
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

	// Current cluster Kubernetes version (from a node's kubelet).
	from := nodes[0].KubeletVersion
	// TODO(M3): choose the next k8s patch the pinned Talos release supports.
	to := from

	// Build the quarantined upgrader from the scratch cluster's creds + the
	// control-plane node IP (the harness helper used elsewhere).
	endpoints := []string{controlPlaneNodeIP(t, c.Name)}
	up, err := k8supgrade.New(c.Talosconfig, endpoints)
	if err != nil {
		t.Fatalf("k8supgrade.New: %v", err)
	}

	// Trigger the Talos-orchestrated upgrade (blocks until Talos reports done).
	if err := up.UpgradeK8s(ctx, from, to); err != nil {
		t.Fatalf("UpgradeK8s(%s -> %s): %v", from, to, err)
	}

	// Monitor-to-completion: Talos drives the upgrade; we observe convergence by
	// polling kubelet versions until every node reports `to`.
	// TODO(M3): wrap this in a bounded poll loop that treats transient
	// control-plane blips as not-yet-converged (park-and-retry), like the OS
	// path's waitHealthy.
	for _, n := range nodes {
		v, err := kc.KubeletVersion(ctx, n.Name)
		if err != nil {
			t.Fatalf("kubelet version %s: %v", n.Name, err)
		}
		if v != to {
			t.Fatalf("node %s at %s, want %s", n.Name, v, to)
		}
	}
}
