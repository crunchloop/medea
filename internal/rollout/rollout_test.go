package rollout

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	durationpb "google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/bilby91/medea/gen/medea/v1"
	"github.com/bilby91/medea/internal/kube"
	"github.com/bilby91/medea/internal/store"
)

// --- stateful fakes ---

type upgradeCall struct{ node, image string }

type fakeTalos struct {
	versions      map[string]string // addr -> current version
	installImage  string
	failUpgrade   map[string]error
	upgrades      []upgradeCall
	snapshots     []string
	snapshotBytes []byte
}

func (f *fakeTalos) Version(_ context.Context, node string) (string, error) {
	return f.versions[node], nil
}
func (f *fakeTalos) InstallImage(_ context.Context, _ string) (string, error) {
	return f.installImage, nil
}
func (f *fakeTalos) UpgradeOS(_ context.Context, node, image string) error {
	if err := f.failUpgrade[node]; err != nil {
		return err
	}
	f.upgrades = append(f.upgrades, upgradeCall{node, image})
	// Applying the image makes the node report the image's version.
	f.versions[node] = tagOf(image)
	return nil
}
func (f *fakeTalos) EtcdSnapshot(_ context.Context, node string, w io.Writer) error {
	f.snapshots = append(f.snapshots, node)
	_, err := w.Write([]byte("ETCD-SNAPSHOT-DATA"))
	return err
}

type fakeKube struct {
	nodes      []kube.NodeInfo
	drainErr   map[string]error
	drained    []string
	uncordoned []string
}

func (f *fakeKube) ListNodes(context.Context) ([]kube.NodeInfo, error) { return f.nodes, nil }
func (f *fakeKube) Drain(_ context.Context, name string, _ time.Duration) error {
	if err := f.drainErr[name]; err != nil {
		return err
	}
	f.drained = append(f.drained, name)
	return nil
}
func (f *fakeKube) Uncordon(_ context.Context, name string) error {
	f.uncordoned = append(f.uncordoned, name)
	return nil
}
func (f *fakeKube) NodeReady(context.Context, string) (bool, error) { return true, nil }

// fakeK8s is a stateful fake of the Kubernetes upgrader. On a successful
// UpgradeK8s it converges every node in the shared fakeKube to the target,
// modelling Talos's orchestration so the reconciler's post-upgrade check passes.
type fakeK8s struct {
	kube    *fakeKube
	calls   []string // "from->to"
	failErr error
}

func (f *fakeK8s) UpgradeK8s(_ context.Context, from, to string) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.calls = append(f.calls, from+"->"+to)
	for i := range f.kube.nodes {
		f.kube.nodes[i].KubeletVersion = to
	}
	return nil
}

func TestReconcileK8sConverges(t *testing.T) {
	st := execStore(t)
	fk := &fakeKube{nodes: []kube.NodeInfo{
		{Name: "cp1", InternalIP: "10.0.0.2", Role: "controlplane", Ready: true, KubeletVersion: "v1.36.1"},
		{Name: "w1", InternalIP: "10.0.0.3", Role: "worker", Ready: true, KubeletVersion: "v1.36.1"},
	}}
	ft := &fakeTalos{versions: map[string]string{}}
	fk8 := &fakeK8s{kube: fk}

	r := New(st, ft, fk, t.TempDir())
	if err := r.ReconcileK8s(context.Background(), "home", "v1.36.2", fk8); err != nil {
		t.Fatalf("ReconcileK8s: %v", err)
	}
	// Snapshot-before-K8s is mandatory and targets the control-plane node.
	if len(ft.snapshots) != 1 || ft.snapshots[0] != "10.0.0.2" {
		t.Fatalf("want one etcd snapshot of the CP node, got %v", ft.snapshots)
	}
	if len(fk8.calls) != 1 || fk8.calls[0] != "v1.36.1->v1.36.2" {
		t.Fatalf("upgrade-k8s not called correctly: %v", fk8.calls)
	}
	cr, _ := st.GetClusterRollout("home")
	if cr.GetPhase() != pb.ClusterRolloutPhase_CLUSTER_ROLLOUT_PHASE_IDLE {
		t.Fatalf("phase = %v, want IDLE", cr.GetPhase())
	}
}

func TestReconcileK8sHaltsOnUpgradeFailure(t *testing.T) {
	st := execStore(t)
	fk := &fakeKube{nodes: []kube.NodeInfo{
		{Name: "cp1", InternalIP: "10.0.0.2", Role: "controlplane", Ready: true, KubeletVersion: "v1.36.1"},
	}}
	ft := &fakeTalos{versions: map[string]string{}}
	fk8 := &fakeK8s{kube: fk, failErr: errors.New("upstream boom")}

	r := New(st, ft, fk, t.TempDir())
	if err := r.ReconcileK8s(context.Background(), "home", "v1.36.2", fk8); err == nil {
		t.Fatal("expected halting error")
	}
	// The snapshot must have been taken before the (failed) upgrade.
	if len(ft.snapshots) != 1 {
		t.Fatalf("snapshot must precede the upgrade even on failure; got %v", ft.snapshots)
	}
	cr, _ := st.GetClusterRollout("home")
	if cr.GetPhase() != pb.ClusterRolloutPhase_CLUSTER_ROLLOUT_PHASE_FAILED {
		t.Fatalf("phase = %v, want FAILED", cr.GetPhase())
	}
}

func TestReconcileK8sSkipsWhenConverged(t *testing.T) {
	st := execStore(t)
	fk := &fakeKube{nodes: []kube.NodeInfo{
		{Name: "cp1", InternalIP: "10.0.0.2", Role: "controlplane", Ready: true, KubeletVersion: "v1.36.2"},
	}}
	ft := &fakeTalos{versions: map[string]string{}}
	fk8 := &fakeK8s{kube: fk}

	r := New(st, ft, fk, t.TempDir())
	if err := r.ReconcileK8s(context.Background(), "home", "v1.36.2", fk8); err != nil {
		t.Fatal(err)
	}
	if len(fk8.calls) != 0 {
		t.Fatalf("must not upgrade when already converged: %v", fk8.calls)
	}
	if len(ft.snapshots) != 0 {
		t.Fatalf("must not snapshot when already converged: %v", ft.snapshots)
	}
}

func tagOf(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i+1:], "/") {
		return image[i+1:]
	}
	return ""
}

// --- helpers ---

func newReconciler(t *testing.T, ft *fakeTalos, fk *fakeKube) (*Reconciler, *store.BoltStore) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := New(st, ft, fk, t.TempDir())
	r.PollInterval = time.Millisecond
	r.WaitTimeout = 2 * time.Second
	return r, st
}

func seedPool(t *testing.T, st *store.BoltStore, role pb.Role, members []string, clusterTarget string, strat *pb.RolloutStrategy) {
	t.Helper()
	if _, err := st.PutClusterDesired(&pb.Cluster{
		Name: "home", Desired: &pb.ClusterDesired{TalosVersion: clusterTarget},
	}, 0); err != nil {
		t.Fatal(err)
	}
	name := "workers"
	if role == pb.Role_ROLE_CONTROLPLANE {
		name = "controlplane"
	}
	if _, err := st.PutNodePoolDesired(&pb.NodePool{
		Cluster: "home", Name: name, Role: role, Members: members,
		Desired: &pb.NodePoolDesired{}, Strategy: strat,
	}, 0); err != nil {
		t.Fatal(err)
	}
}

func workerNodes(ips ...string) []kube.NodeInfo {
	var out []kube.NodeInfo
	for i, ip := range ips {
		out = append(out, kube.NodeInfo{Name: "w" + string(rune('1'+i)), InternalIP: ip, Role: "worker", Ready: true})
	}
	return out
}

// --- tests ---

func TestUpgradesOutdatedNodes(t *testing.T) {
	ft := &fakeTalos{
		versions:     map[string]string{"10.0.0.3": "v1.13.5", "10.0.0.4": "v1.13.5"},
		installImage: "ghcr.io/siderolabs/installer:v1.13.5",
	}
	fk := &fakeKube{nodes: workerNodes("10.0.0.3", "10.0.0.4")}
	r, st := newReconciler(t, ft, fk)
	seedPool(t, st, pb.Role_ROLE_WORKER, []string{"10.0.0.3", "10.0.0.4"}, "v1.13.6", &pb.RolloutStrategy{MaxUnavailable: 1})

	if err := r.ReconcilePool(context.Background(), "home", "workers"); err != nil {
		t.Fatalf("ReconcilePool: %v", err)
	}

	if len(ft.upgrades) != 2 {
		t.Fatalf("expected 2 upgrades, got %d: %+v", len(ft.upgrades), ft.upgrades)
	}
	for _, u := range ft.upgrades {
		if u.image != "ghcr.io/siderolabs/installer:v1.13.6" {
			t.Fatalf("wrong image: %q", u.image)
		}
	}
	if len(fk.drained) != 2 || len(fk.uncordoned) != 2 {
		t.Fatalf("drain/uncordon counts: %d/%d", len(fk.drained), len(fk.uncordoned))
	}
	for _, addr := range []string{"10.0.0.3", "10.0.0.4"} {
		mr, _ := st.GetMachineRollout("home", addr)
		if mr.GetState() != pb.RolloutState_ROLLOUT_STATE_DONE {
			t.Fatalf("%s state = %v, want DONE", addr, mr.GetState())
		}
	}
}

func TestSkipsAlreadyCurrentNode(t *testing.T) {
	ft := &fakeTalos{
		versions:     map[string]string{"10.0.0.3": "v1.13.6", "10.0.0.4": "v1.13.5"},
		installImage: "ghcr.io/siderolabs/installer:v1.13.5",
	}
	fk := &fakeKube{nodes: workerNodes("10.0.0.3", "10.0.0.4")}
	r, st := newReconciler(t, ft, fk)
	seedPool(t, st, pb.Role_ROLE_WORKER, []string{"10.0.0.3", "10.0.0.4"}, "v1.13.6", &pb.RolloutStrategy{MaxUnavailable: 1})

	if err := r.ReconcilePool(context.Background(), "home", "workers"); err != nil {
		t.Fatal(err)
	}
	if len(ft.upgrades) != 1 || ft.upgrades[0].node != "10.0.0.4" {
		t.Fatalf("expected only 10.0.0.4 upgraded, got %+v", ft.upgrades)
	}
}

func TestHaltOnDrainFailure(t *testing.T) {
	ft := &fakeTalos{
		versions:     map[string]string{"10.0.0.3": "v1.13.5", "10.0.0.4": "v1.13.5"},
		installImage: "ghcr.io/siderolabs/installer:v1.13.5",
	}
	fk := &fakeKube{
		nodes:    workerNodes("10.0.0.3", "10.0.0.4"),
		drainErr: map[string]error{"w1": errors.New("PDB blocked")}, // w1 == 10.0.0.3 (first)
	}
	r, st := newReconciler(t, ft, fk)
	seedPool(t, st, pb.Role_ROLE_WORKER, []string{"10.0.0.3", "10.0.0.4"}, "v1.13.6", &pb.RolloutStrategy{MaxUnavailable: 1})

	err := r.ReconcilePool(context.Background(), "home", "workers")
	if err == nil {
		t.Fatal("expected halt error")
	}
	// First node Failed; no upgrade attempted; second node never processed.
	mr1, _ := st.GetMachineRollout("home", "10.0.0.3")
	if mr1.GetState() != pb.RolloutState_ROLLOUT_STATE_FAILED {
		t.Fatalf("first node state = %v, want FAILED", mr1.GetState())
	}
	if len(ft.upgrades) != 0 {
		t.Fatalf("no upgrade should have run, got %+v", ft.upgrades)
	}
	if mr2, _ := st.GetMachineRollout("home", "10.0.0.4"); mr2 != nil {
		t.Fatalf("second node should not be touched, got %+v", mr2)
	}
}

func TestControlPlaneSnapshotBeforeUpgrade(t *testing.T) {
	ft := &fakeTalos{
		versions:     map[string]string{"10.0.0.2": "v1.13.5"},
		installImage: "factory.talos.dev/installer/abc:v1.13.5",
	}
	fk := &fakeKube{nodes: []kube.NodeInfo{{Name: "cp", InternalIP: "10.0.0.2", Role: "controlplane", Ready: true}}}
	r, st := newReconciler(t, ft, fk)
	snapDir := r.snapshotDir
	seedPool(t, st, pb.Role_ROLE_CONTROLPLANE, []string{"10.0.0.2"}, "v1.13.6",
		&pb.RolloutStrategy{MaxUnavailable: 1, SnapshotBeforeControlPlane: true, DrainTimeout: durationpb.New(time.Minute)})

	if err := r.ReconcilePool(context.Background(), "home", "controlplane"); err != nil {
		t.Fatalf("ReconcilePool: %v", err)
	}

	// Snapshot taken before the upgrade, and persisted to disk.
	if len(ft.snapshots) != 1 || ft.snapshots[0] != "10.0.0.2" {
		t.Fatalf("snapshot not taken: %+v", ft.snapshots)
	}
	snapFile := filepath.Join(snapDir, "home-10.0.0.2.snapshot")
	if b, err := os.ReadFile(snapFile); err != nil || len(b) == 0 {
		t.Fatalf("snapshot file missing/empty: %v", err)
	}
	// Schematic preserved in the upgrade image.
	if len(ft.upgrades) != 1 || ft.upgrades[0].image != "factory.talos.dev/installer/abc:v1.13.6" {
		t.Fatalf("upgrade image wrong: %+v", ft.upgrades)
	}
}
