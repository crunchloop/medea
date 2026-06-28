// Package rollout is the version-rollout reconciler. This file implements the
// OS path (rollout-controller.md §2.1): for a node pool, upgrade each node whose
// Talos version differs from the target — drain, snapshot-before-control-plane,
// UpgradeOS (reboot), wait-healthy, uncordon — halting the whole rollout on the
// first node that fails to converge.
//
// Processing is sequential (one node in flight at a time), so at most one pool
// member is unavailable at once — which satisfies any maxUnavailable >= 1.
// Parallelism is deferred (rollout-controller.md §6).
package rollout

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pb "github.com/bilby91/medea/gen/medea/v1"
	"github.com/bilby91/medea/internal/kube"
	"github.com/bilby91/medea/internal/store"
	"github.com/bilby91/medea/internal/talos"
)

// TalosOps and KubeOps are the slices of the talos/kube clients the reconciler
// drives; narrowing to interfaces keeps it unit-testable with fakes
// (talos-client.md §1).
type TalosOps interface {
	Version(ctx context.Context, node string) (string, error)
	InstallImage(ctx context.Context, node string) (string, error)
	UpgradeOS(ctx context.Context, node, image string) error
	EtcdSnapshot(ctx context.Context, node string, w io.Writer) error
}

type KubeOps interface {
	ListNodes(ctx context.Context) ([]kube.NodeInfo, error)
	Drain(ctx context.Context, name string, timeout time.Duration) error
	Uncordon(ctx context.Context, name string) error
	NodeReady(ctx context.Context, name string) (bool, error)
}

// Reconciler drives OS rollouts for a cluster's pools.
type Reconciler struct {
	store       store.Store
	talos       TalosOps
	kube        KubeOps
	snapshotDir string

	// tunables (small in tests)
	PollInterval time.Duration
	WaitTimeout  time.Duration
}

// New returns a Reconciler. snapshotDir is where pre-control-plane etcd
// snapshots are written.
func New(st store.Store, t TalosOps, k KubeOps, snapshotDir string) *Reconciler {
	return &Reconciler{
		store: st, talos: t, kube: k, snapshotDir: snapshotDir,
		PollInterval: 5 * time.Second,
		WaitTimeout:  10 * time.Minute,
	}
}

// ReconcilePool brings every node in cluster/pool to the target Talos version.
// It returns nil when the pool is converged (or paused), and an error (after
// marking the offending node Failed) on the first node that cannot converge —
// halt-on-failure (rollout-controller.md §3).
func (r *Reconciler) ReconcilePool(ctx context.Context, cluster, pool string) error {
	np, _, err := r.store.GetNodePool(cluster, pool)
	if err != nil {
		return err
	}
	if np == nil {
		return fmt.Errorf("rollout: nodepool %q/%q not found", cluster, pool)
	}
	if np.GetPaused() {
		return nil
	}

	target, err := r.targetVersion(cluster, np)
	if err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("rollout: no target Talos version for %s/%s", cluster, pool)
	}

	nodes, err := r.kube.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	ipToName := make(map[string]string, len(nodes))
	ready := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		ipToName[n.InternalIP] = n.Name
		ready[n.InternalIP] = n.Ready
	}

	members := append([]string(nil), np.GetMembers()...)
	sort.Strings(members) // deterministic order

	maxUnavail := int(np.GetStrategy().GetMaxUnavailable())
	if maxUnavail < 1 {
		maxUnavail = 1
	}

	for _, addr := range members {
		cur, err := r.talos.Version(ctx, addr)
		if err != nil {
			return r.fail(cluster, addr, target, fmt.Sprintf("read version: %v", err))
		}
		if cur == target {
			r.setState(cluster, addr, pb.RolloutState_ROLLOUT_STATE_DONE, target, "")
			continue
		}

		// Budget: don't start if too many *other* members are already unavailable.
		if unavailableOthers(ready, members, addr) >= maxUnavail {
			// Not safe to proceed this pass; leave for a later reconcile.
			return nil
		}

		if err := r.upgradeNode(ctx, cluster, np.GetRole(), addr, ipToName[addr], target, np.GetStrategy()); err != nil {
			return err // node already marked Failed; halt the rollout
		}
	}
	return nil
}

// ReconcileK8s drives the cluster-wide Kubernetes upgrade (rollout-controller.md
// §2.2): take a mandatory etcd snapshot (the K8s upgrade touches control-plane
// components — the only undo on non-HA), then trigger Talos's upgrade-k8s and
// verify convergence. Talos orchestrates the disruption itself (control-plane
// static pods + kubelets); Medea triggers it and confirms every node reached the
// target. Progress is tracked in the cluster's ClusterRollout record.
func (r *Reconciler) ReconcileK8s(ctx context.Context, cluster, target string, k8s K8sOps) error {
	if target == "" {
		return fmt.Errorf("rollout: no target Kubernetes version for %s", cluster)
	}
	r.setClusterPhase(cluster, pb.ClusterRolloutPhase_CLUSTER_ROLLOUT_PHASE_UPGRADING, target, "upgrading")

	nodes, err := r.kube.ListNodes(ctx)
	if err != nil {
		return r.failCluster(cluster, target, fmt.Sprintf("list nodes: %v", err))
	}
	var cpIP, from string
	for _, n := range nodes {
		if n.Role == "controlplane" {
			cpIP, from = n.InternalIP, n.KubeletVersion
		}
	}
	if cpIP == "" {
		return r.failCluster(cluster, target, "no control-plane node found")
	}
	if sameVersion(from, target) {
		// Already converged — nothing to do.
		r.setClusterPhase(cluster, pb.ClusterRolloutPhase_CLUSTER_ROLLOUT_PHASE_IDLE, target, "")
		return nil
	}

	// Snapshot etcd before mutating the control plane — mandatory; failure aborts
	// before any upgrade (rollout-safety.md §4, talos-client.md §5).
	if err := r.snapshot(ctx, cluster, cpIP); err != nil {
		return r.failCluster(cluster, target, fmt.Sprintf("etcd snapshot: %v", err))
	}

	// Talos-orchestrated upgrade; blocks to completion.
	if err := k8s.UpgradeK8s(ctx, from, target); err != nil {
		return r.failCluster(cluster, target, fmt.Sprintf("upgrade-k8s: %v", err))
	}

	// Verify every node converged to the target kubelet version.
	nodes, err = r.kube.ListNodes(ctx)
	if err != nil {
		return r.failCluster(cluster, target, fmt.Sprintf("post-upgrade list nodes: %v", err))
	}
	for _, n := range nodes {
		if !sameVersion(n.KubeletVersion, target) {
			return r.failCluster(cluster, target, fmt.Sprintf("node %s at %s, want %s", n.Name, n.KubeletVersion, target))
		}
	}

	r.setClusterPhase(cluster, pb.ClusterRolloutPhase_CLUSTER_ROLLOUT_PHASE_IDLE, target, "")
	return nil
}

func (r *Reconciler) setClusterPhase(cluster string, phase pb.ClusterRolloutPhase, target, msg string) {
	_ = r.store.PutClusterRollout(&pb.ClusterRollout{
		Cluster: cluster, Phase: phase, TargetKubernetesVersion: target, Message: msg,
	})
}

// failCluster marks the cluster's K8s rollout Failed and returns a halting error.
func (r *Reconciler) failCluster(cluster, target, msg string) error {
	r.setClusterPhase(cluster, pb.ClusterRolloutPhase_CLUSTER_ROLLOUT_PHASE_FAILED, target, msg)
	return fmt.Errorf("k8s rollout halted: %s", msg)
}

// sameVersion compares Kubernetes versions ignoring a leading "v" (kubelet
// reports "v1.36.2"; a target may be given either way).
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// upgradeNode runs one node through the OS-path state machine.
func (r *Reconciler) upgradeNode(ctx context.Context, cluster string, role pb.Role, addr, name, target string, strat *pb.RolloutStrategy) error {
	if name == "" {
		return r.fail(cluster, addr, target, "node not found in cluster (cannot drain)")
	}

	// Snapshot before mutating a control-plane node — the only undo on non-HA.
	if role == pb.Role_ROLE_CONTROLPLANE && strat.GetSnapshotBeforeControlPlane() {
		if err := r.snapshot(ctx, cluster, addr); err != nil {
			return r.fail(cluster, addr, target, fmt.Sprintf("etcd snapshot: %v", err))
		}
	}

	// Drain (cordons internally; PDB-respecting; no force).
	r.setState(cluster, addr, pb.RolloutState_ROLLOUT_STATE_DRAINING, target, "draining")
	drainTimeout := strat.GetDrainTimeout().AsDuration()
	if drainTimeout <= 0 {
		drainTimeout = 5 * time.Minute
	}
	if err := r.kube.Drain(ctx, name, drainTimeout); err != nil {
		return r.fail(cluster, addr, target, fmt.Sprintf("drain: %v", err))
	}

	// Upgrade (reboots), preserving the node's schematic.
	r.setState(cluster, addr, pb.RolloutState_ROLLOUT_STATE_UPGRADING, target, "upgrading")
	curImage, err := r.talos.InstallImage(ctx, addr)
	if err != nil {
		return r.fail(cluster, addr, target, fmt.Sprintf("read install image: %v", err))
	}
	image := talos.DeriveInstallerImage(curImage, target)
	if err := r.talos.UpgradeOS(ctx, addr, image); err != nil {
		return r.fail(cluster, addr, target, fmt.Sprintf("upgrade: %v", err))
	}

	// Wait until the node returns Ready and reports the target version.
	r.setState(cluster, addr, pb.RolloutState_ROLLOUT_STATE_WAITING_HEALTHY, target, "waiting for healthy")
	if err := r.waitHealthy(ctx, addr, name, target); err != nil {
		return r.fail(cluster, addr, target, err.Error())
	}

	if err := r.kube.Uncordon(ctx, name); err != nil {
		return r.fail(cluster, addr, target, fmt.Sprintf("uncordon: %v", err))
	}
	r.setState(cluster, addr, pb.RolloutState_ROLLOUT_STATE_DONE, target, "")
	return nil
}

// waitHealthy polls until the node is Ready and at the target version, or times
// out. Connection errors during the reboot are expected and treated as
// not-ready-yet (rollout-controller.md §4).
func (r *Reconciler) waitHealthy(ctx context.Context, addr, name, target string) error {
	wctx, cancel := context.WithTimeout(ctx, r.WaitTimeout)
	defer cancel()
	for {
		ready, kerr := r.kube.NodeReady(wctx, name)
		v, verr := r.talos.Version(wctx, addr)
		if ready && v == target {
			return nil
		}
		log.Printf("rollout: waiting %s -> %s (ready=%t version=%q kubeErr=%v talosErr=%v)",
			addr, target, ready, v, kerr, verr)
		select {
		case <-wctx.Done():
			return fmt.Errorf("timed out waiting for %s to reach %s (ready=%t, version=%q)", addr, target, ready, v)
		case <-time.After(r.PollInterval):
		}
	}
}

func (r *Reconciler) snapshot(ctx context.Context, cluster, addr string) error {
	if err := os.MkdirAll(r.snapshotDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(r.snapshotDir, fmt.Sprintf("%s-%s.snapshot", cluster, addr))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := r.talos.EtcdSnapshot(ctx, addr, f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (r *Reconciler) targetVersion(cluster string, np *pb.NodePool) (string, error) {
	if v := np.GetDesired().GetTalosVersion(); v != "" {
		return v, nil
	}
	cl, _, err := r.store.GetCluster(cluster)
	if err != nil {
		return "", err
	}
	if cl == nil {
		return "", fmt.Errorf("rollout: cluster %q not found", cluster)
	}
	return cl.GetDesired().GetTalosVersion(), nil
}

func (r *Reconciler) setState(cluster, addr string, state pb.RolloutState, target, msg string) {
	_ = r.store.PutMachineRollout(&pb.MachineRollout{
		Cluster: cluster, Addr: addr, State: state, TargetTalosVersion: target, Message: msg,
	})
}

// fail marks the node's rollout Failed and returns an error to halt the pool.
func (r *Reconciler) fail(cluster, addr, target, msg string) error {
	r.setState(cluster, addr, pb.RolloutState_ROLLOUT_STATE_FAILED, target, msg)
	return fmt.Errorf("rollout halted at %s: %s", addr, msg)
}

func unavailableOthers(ready map[string]bool, members []string, self string) int {
	n := 0
	for _, m := range members {
		if m == self {
			continue
		}
		if r, ok := ready[m]; !ok || !r {
			n++
		}
	}
	return n
}
