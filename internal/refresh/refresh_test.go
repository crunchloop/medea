package refresh

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/bilby91/medea/gen/medea/v1"
	"github.com/bilby91/medea/internal/kube"
	"github.com/bilby91/medea/internal/store"
)

type fakeKube struct {
	nodes []kube.NodeInfo
	err   error
}

func (f fakeKube) ListNodes(context.Context) ([]kube.NodeInfo, error) { return f.nodes, f.err }

type fakeTalos struct {
	versions map[string]string
}

func (f fakeTalos) Version(_ context.Context, node string) (string, error) {
	v, ok := f.versions[node]
	if !ok {
		return "", errors.New("unreachable")
	}
	return v, nil
}

func openStore(t *testing.T) *store.BoltStore {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seed(t *testing.T, st *store.BoltStore) {
	t.Helper()
	if _, err := st.PutClusterDesired(&pb.Cluster{Name: "home", Desired: &pb.ClusterDesired{}}, 0); err != nil {
		t.Fatal(err)
	}
	for _, m := range []*pb.Machine{
		{Cluster: "home", Pool: "controlplane", TalosEndpoint: "10.5.0.2", Role: pb.Role_ROLE_CONTROLPLANE},
		{Cluster: "home", Pool: "workers", TalosEndpoint: "10.5.0.3", Role: pb.Role_ROLE_WORKER},
	} {
		if _, err := st.PutMachineDesired(m, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOncePopulatesObserved(t *testing.T) {
	st := openStore(t)
	seed(t, st)

	factory := func(context.Context, *pb.Cluster) (*Clients, error) {
		return &Clients{
			Kube: fakeKube{nodes: []kube.NodeInfo{
				{Name: "cp", InternalIP: "10.5.0.2", Role: "controlplane", KubeletVersion: "v1.36.1", Ready: true},
				{Name: "w1", InternalIP: "10.5.0.3", Role: "worker", KubeletVersion: "v1.36.1", Ready: true},
			}},
			Talos: fakeTalos{versions: map[string]string{"10.5.0.2": "v1.13.5", "10.5.0.3": "v1.13.5"}},
		}, nil
	}
	r := New(st, factory, time.Minute)
	if err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}

	// Machine observed populated.
	m, _, _ := st.GetMachine("home", "10.5.0.2")
	o := m.GetObserved()
	if o.GetTalosVersion() != "v1.13.5" || o.GetKubernetesVersion() != "v1.36.1" || !o.GetHealthy() {
		t.Fatalf("cp observed wrong: %+v", o)
	}
	if o.GetPhase() != pb.MachinePhase_MACHINE_PHASE_READY {
		t.Fatalf("cp phase = %v", o.GetPhase())
	}

	// Cluster observed from control-plane node.
	cl, _, _ := st.GetCluster("home")
	if cl.GetObserved().GetKubernetesVersion() != "v1.36.1" || !cl.GetObserved().GetControlPlaneReady() {
		t.Fatalf("cluster observed wrong: %+v", cl.GetObserved())
	}
}

func TestUnreachableTalosLeavesHealthyFalse(t *testing.T) {
	st := openStore(t)
	seed(t, st)

	// Node is Ready in kube, but Talos is unreachable (mid-reboot scenario).
	factory := func(context.Context, *pb.Cluster) (*Clients, error) {
		return &Clients{
			Kube: fakeKube{nodes: []kube.NodeInfo{
				{Name: "w1", InternalIP: "10.5.0.3", Role: "worker", KubeletVersion: "v1.36.1", Ready: true},
			}},
			Talos: fakeTalos{versions: map[string]string{}}, // no versions -> error
		}, nil
	}
	r := New(st, factory, time.Minute)
	if err := r.Once(context.Background()); err != nil {
		t.Fatalf("Once: %v", err)
	}
	m, _, _ := st.GetMachine("home", "10.5.0.3")
	if m.GetObserved().GetTalosVersion() != "" {
		t.Fatalf("expected blank talos version, got %q", m.GetObserved().GetTalosVersion())
	}
	if m.GetObserved().GetHealthy() {
		t.Fatal("expected healthy=false when Talos unreachable")
	}
}

func TestClusterUnreachableMarksCPNotReady(t *testing.T) {
	st := openStore(t)
	seed(t, st)
	factory := func(context.Context, *pb.Cluster) (*Clients, error) {
		return &Clients{Kube: fakeKube{err: errors.New("apiserver down")}, Talos: fakeTalos{}}, nil
	}
	r := New(st, factory, time.Minute)
	if err := r.Once(context.Background()); err == nil {
		t.Fatal("expected error when cluster unreachable")
	}
	cl, _, _ := st.GetCluster("home")
	if cl.GetObserved().GetControlPlaneReady() {
		t.Fatal("expected control_plane_ready=false")
	}
}
