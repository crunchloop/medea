package provision

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/bilby91/medea/gen/medea/v1"
	"github.com/bilby91/medea/internal/kube"
	"github.com/bilby91/medea/internal/store"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
)

type fakeProv struct {
	staged   []string
	unstaged []string
}

func (f *fakeProv) Stage(_ context.Context, mac string, _ Profile, _ []byte) error {
	f.staged = append(f.staged, mac)
	return nil
}
func (f *fakeProv) Unstage(_ context.Context, mac string) error {
	f.unstaged = append(f.unstaged, mac)
	return nil
}

type fakeResolver struct{ id string }

func (f fakeResolver) Resolve(context.Context, []string) (string, error) { return f.id, nil }

type fakeKube struct {
	nodes   []kube.NodeInfo
	drained []string
}

func (f *fakeKube) ListNodes(context.Context) ([]kube.NodeInfo, error) { return f.nodes, nil }
func (f *fakeKube) Drain(_ context.Context, name string, _ time.Duration) error {
	f.drained = append(f.drained, name)
	return nil
}

func provStore(t *testing.T) *store.BoltStore {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedProv(t *testing.T, st *store.BoltStore, enabled bool, hostState pb.HostState) {
	t.Helper()
	if _, err := st.PutClusterDesired(&pb.Cluster{
		Name:                "home",
		Desired:             &pb.ClusterDesired{TalosVersion: "v1.13.5", KubernetesVersion: "v1.36.2"},
		Endpoints:           &pb.ClusterEndpoints{Kube: "10.5.0.2:6443"},
		ProvisioningEnabled: enabled,
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutNodePoolDesired(&pb.NodePool{
		Cluster: "home", Name: "workers", Role: pb.Role_ROLE_WORKER,
		Replicas: 1, Selector: map[string]string{"role": "worker"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutHostDesired(&pb.Host{
		Cluster: "home", Mac: "aa:bb", Pool: "workers", Role: pb.Role_ROLE_WORKER,
		Labels: map[string]string{"role": "worker"}, State: hostState,
	}, 0); err != nil {
		t.Fatal(err)
	}
}

func newRec(t *testing.T, st *store.BoltStore, p Provisioner, k KubeOps) *Reconciler {
	return NewReconciler(st, p, fakeResolver{id: "schem123"}, k,
		func(string) (*secrets.Bundle, error) { return testBundle(t), nil },
		"factory.talos.dev", "/dev/sda")
}

// Only the control-plane node exists yet — no worker has joined.
func cpOnly() *fakeKube {
	return &fakeKube{nodes: []kube.NodeInfo{{Name: "cp1", InternalIP: "10.5.0.2", Role: "controlplane", Ready: true}}}
}

func TestReconcileGuardDisabled(t *testing.T) {
	st := provStore(t)
	seedProv(t, st, false, pb.HostState_HOST_STATE_REGISTERED) // NOT enabled
	p := &fakeProv{}
	if err := newRec(t, st, p, cpOnly()).ReconcilePool(context.Background(), "home", "workers"); err != nil {
		t.Fatal(err)
	}
	if len(p.staged) != 0 {
		t.Fatalf("staged on a disabled cluster: %v", p.staged)
	}
	h, _, _ := st.GetHost("home", "aa:bb")
	if h.GetState() != pb.HostState_HOST_STATE_REGISTERED {
		t.Fatalf("host state changed while disabled: %v", h.GetState())
	}
}

func TestReconcileScaleOutStages(t *testing.T) {
	st := provStore(t)
	seedProv(t, st, true, pb.HostState_HOST_STATE_REGISTERED)
	p := &fakeProv{}
	if err := newRec(t, st, p, cpOnly()).ReconcilePool(context.Background(), "home", "workers"); err != nil {
		t.Fatal(err)
	}
	if len(p.staged) != 1 || p.staged[0] != "aa:bb" {
		t.Fatalf("expected one stage of aa:bb, got %v", p.staged)
	}
	h, _, _ := st.GetHost("home", "aa:bb")
	if h.GetState() != pb.HostState_HOST_STATE_PROVISIONING {
		t.Fatalf("host state = %v, want PROVISIONING", h.GetState())
	}
}

func TestReconcileBindsOnJoin(t *testing.T) {
	st := provStore(t)
	seedProv(t, st, true, pb.HostState_HOST_STATE_PROVISIONING) // already staged
	p := &fakeProv{}
	// The provisioned worker has now joined.
	k := &fakeKube{nodes: []kube.NodeInfo{
		{Name: "cp1", InternalIP: "10.5.0.2", Role: "controlplane", Ready: true},
		{Name: "w1", InternalIP: "10.5.0.3", Role: "worker", Ready: true},
	}}
	if err := newRec(t, st, p, k).ReconcilePool(context.Background(), "home", "workers"); err != nil {
		t.Fatal(err)
	}
	if len(p.staged) != 0 {
		t.Fatalf("should not stage while binding: %v", p.staged)
	}
	h, _, _ := st.GetHost("home", "aa:bb")
	if h.GetState() != pb.HostState_HOST_STATE_READY || h.GetAddr() != "10.5.0.3" {
		t.Fatalf("host not bound: %+v", h)
	}
	if m, _, _ := st.GetMachine("home", "10.5.0.3"); m == nil || m.GetPool() != "workers" {
		t.Fatalf("machine not created: %+v", m)
	}
	np, _, _ := st.GetNodePool("home", "workers")
	if !contains(np.GetMembers(), "10.5.0.3") {
		t.Fatalf("member not added: %v", np.GetMembers())
	}
}

func TestReconcileNoCapacity(t *testing.T) {
	st := provStore(t)
	// Enabled + replicas=1 but the only host is already Ready elsewhere / none Registered.
	if _, err := st.PutClusterDesired(&pb.Cluster{
		Name: "home", Desired: &pb.ClusterDesired{TalosVersion: "v1.13.5"},
		Endpoints: &pb.ClusterEndpoints{Kube: "10.5.0.2:6443"}, ProvisioningEnabled: true,
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutNodePoolDesired(&pb.NodePool{
		Cluster: "home", Name: "workers", Role: pb.Role_ROLE_WORKER, Replicas: 1,
		Selector: map[string]string{"role": "worker"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	p := &fakeProv{}
	if err := newRec(t, st, p, cpOnly()).ReconcilePool(context.Background(), "home", "workers"); err != nil {
		t.Fatal(err)
	}
	if len(p.staged) != 0 {
		t.Fatalf("staged with no available host: %v", p.staged)
	}
}

func TestReconcileScaleIn(t *testing.T) {
	st := provStore(t)
	if _, err := st.PutClusterDesired(&pb.Cluster{
		Name: "home", Desired: &pb.ClusterDesired{TalosVersion: "v1.13.5"},
		Endpoints: &pb.ClusterEndpoints{Kube: "10.5.0.2:6443"}, ProvisioningEnabled: true,
	}, 0); err != nil {
		t.Fatal(err)
	}
	// replicas=1 but two Ready hosts -> scale in one (victim = lowest MAC = aa:bb).
	if _, err := st.PutNodePoolDesired(&pb.NodePool{
		Cluster: "home", Name: "workers", Role: pb.Role_ROLE_WORKER, Replicas: 1,
		Members: []string{"10.5.0.3", "10.5.0.4"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	for _, h := range []struct{ mac, addr string }{{"aa:bb", "10.5.0.3"}, {"cc:dd", "10.5.0.4"}} {
		if _, err := st.PutHostDesired(&pb.Host{
			Cluster: "home", Mac: h.mac, Pool: "workers", Role: pb.Role_ROLE_WORKER,
			Addr: h.addr, State: pb.HostState_HOST_STATE_READY,
		}, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutMachineDesired(&pb.Machine{Cluster: "home", Pool: "workers", TalosEndpoint: h.addr, Role: pb.Role_ROLE_WORKER}, 0); err != nil {
			t.Fatal(err)
		}
	}

	p := &fakeProv{}
	k := &fakeKube{nodes: []kube.NodeInfo{
		{Name: "cp1", InternalIP: "10.5.0.2", Role: "controlplane", Ready: true},
		{Name: "w1", InternalIP: "10.5.0.3", Role: "worker", Ready: true},
		{Name: "w2", InternalIP: "10.5.0.4", Role: "worker", Ready: true},
	}}
	if err := newRec(t, st, p, k).ReconcilePool(context.Background(), "home", "workers"); err != nil {
		t.Fatal(err)
	}

	// Victim aa:bb (10.5.0.3): drained, unstaged, machine+member removed, released.
	if len(k.drained) != 1 || k.drained[0] != "w1" {
		t.Fatalf("expected w1 drained, got %v", k.drained)
	}
	if len(p.unstaged) != 1 || p.unstaged[0] != "aa:bb" {
		t.Fatalf("expected aa:bb unstaged, got %v", p.unstaged)
	}
	h, _, _ := st.GetHost("home", "aa:bb")
	if h.GetState() != pb.HostState_HOST_STATE_REGISTERED || h.GetAddr() != "" {
		t.Fatalf("victim not released: %+v", h)
	}
	if m, _, _ := st.GetMachine("home", "10.5.0.3"); m != nil {
		t.Fatalf("victim machine not deleted: %+v", m)
	}
	np, _, _ := st.GetNodePool("home", "workers")
	if contains(np.GetMembers(), "10.5.0.3") || !contains(np.GetMembers(), "10.5.0.4") {
		t.Fatalf("members wrong after scale-in: %v", np.GetMembers())
	}
	// The other host is untouched.
	if h2, _, _ := st.GetHost("home", "cc:dd"); h2.GetState() != pb.HostState_HOST_STATE_READY {
		t.Fatalf("non-victim changed: %+v", h2)
	}
}
