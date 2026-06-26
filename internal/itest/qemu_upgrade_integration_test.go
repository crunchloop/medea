//go:build integration

package itest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/bilby91/medea/gen/medea/v1"
	"github.com/bilby91/medea/internal/kube"
	"github.com/bilby91/medea/internal/rollout"
	"github.com/bilby91/medea/internal/store"
	"github.com/bilby91/medea/internal/talos"
)

// TestQemuUpgrade is the faithful end-to-end rollout test: it drives the REAL
// reconciler (real talos.UpgradeOS, A/B reboot, real drain) against a QEMU VM to
// upgrade a worker to a target Talos version and confirms it comes back at that
// version and Ready. This is the path the docker provisioner cannot validate.
//
// It connects to a cluster created out-of-band (QEMU needs sudo); see
// scripts/qemu-validate.sh, which sets the MEDEA_QEMU_* env. Skipped otherwise,
// so the normal docker integration run is unaffected.
func TestQemuUpgrade(t *testing.T) {
	talosPath := os.Getenv("MEDEA_QEMU_TALOSCONFIG")
	kubePath := os.Getenv("MEDEA_QEMU_KUBECONFIG")
	target := os.Getenv("MEDEA_QEMU_TARGET")
	if talosPath == "" || kubePath == "" || target == "" {
		t.Skip("MEDEA_QEMU_{TALOSCONFIG,KUBECONFIG,TARGET} not set; run scripts/qemu-validate.sh")
	}

	tb, err := os.ReadFile(talosPath)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := os.ReadFile(kubePath)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	kc, err := kube.New(kb)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := talos.New(ctx, tb, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()

	nodes, err := kc.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var workerIP, workerName string
	for _, n := range nodes {
		if n.Role == "worker" {
			workerIP, workerName = n.InternalIP, n.Name
		}
	}
	if workerIP == "" {
		t.Fatal("no worker node in the qemu cluster")
	}

	cur, err := tc.Version(ctx, workerIP)
	if err != nil {
		t.Fatalf("read worker version: %v", err)
	}
	if cur == target {
		t.Fatalf("worker already at target %s; pick a different MEDEA_QEMU_TARGET", target)
	}
	t.Logf("worker %s (%s): %s -> %s", workerName, workerIP, cur, target)

	// Model the cluster in a fresh store, desired = target.
	st, err := store.Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.PutClusterDesired(&pb.Cluster{
		Name: "qemu", Desired: &pb.ClusterDesired{TalosVersion: target},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutNodePoolDesired(&pb.NodePool{
		Cluster: "qemu", Name: "workers", Role: pb.Role_ROLE_WORKER,
		Members: []string{workerIP}, Desired: &pb.NodePoolDesired{TalosVersion: target},
		Strategy: &pb.RolloutStrategy{MaxUnavailable: 1},
	}, 0); err != nil {
		t.Fatal(err)
	}

	// Drive the real reconciler (UpgradeOS reboots the VM).
	r := rollout.New(st, tc, kc, t.TempDir())
	r.PollInterval = 10 * time.Second
	r.WaitTimeout = 20 * time.Minute
	if err := r.ReconcilePool(ctx, "qemu", "workers"); err != nil {
		t.Fatalf("ReconcilePool: %v", err)
	}

	// Verify convergence.
	got, err := tc.Version(ctx, workerIP)
	if err != nil {
		t.Fatalf("post-upgrade version: %v", err)
	}
	if got != target {
		t.Fatalf("worker at %s after rollout, want %s", got, target)
	}
	if ready, _ := kc.NodeReady(ctx, workerName); !ready {
		t.Fatalf("worker %s not Ready after rollout", workerName)
	}
}
