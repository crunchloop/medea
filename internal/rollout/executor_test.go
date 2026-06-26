package rollout

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

func execStore(t *testing.T) *store.BoltStore {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedClusterPoolJob(t *testing.T, st *store.BoltStore, enabled bool) {
	t.Helper()
	if _, err := st.PutClusterDesired(&pb.Cluster{
		Name: "home", Desired: &pb.ClusterDesired{TalosVersion: "v1.13.6"}, RolloutsEnabled: enabled,
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutNodePoolDesired(&pb.NodePool{
		Cluster: "home", Name: "workers", Role: pb.Role_ROLE_WORKER,
		Members: []string{"10.0.0.3"}, Desired: &pb.NodePoolDesired{TalosVersion: "v1.13.6"},
		Strategy: &pb.RolloutStrategy{MaxUnavailable: 1},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRolloutJob(&pb.Rollout{
		Cluster: "home", Pool: "workers", Kind: pb.RolloutKind_ROLLOUT_KIND_TALOS,
		TargetVersion: "v1.13.6", State: pb.RolloutJobState_ROLLOUT_JOB_STATE_PENDING,
	}); err != nil {
		t.Fatal(err)
	}
}

// THE critical safety test: a cluster that isn't enabled never executes, even
// with a Pending job present and the executor running.
func TestExecutorSkipsDisabledCluster(t *testing.T) {
	st := execStore(t)
	seedClusterPoolJob(t, st, false) // NOT enabled

	called := false
	factory := func(context.Context, *pb.Cluster) (TalosOps, KubeOps, func(), error) {
		called = true
		return nil, nil, nil, nil
	}
	e := NewExecutor(st, factory, t.TempDir(), time.Minute)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("executor built clients for a disabled cluster")
	}
	job, _ := st.GetRolloutJob("home", "workers")
	if job.GetState() != pb.RolloutJobState_ROLLOUT_JOB_STATE_PENDING {
		t.Fatalf("disabled-cluster job changed state to %v", job.GetState())
	}
}

func TestExecutorRunsJobOnEnabledCluster(t *testing.T) {
	st := execStore(t)
	seedClusterPoolJob(t, st, true)

	ft := &fakeTalos{
		versions:     map[string]string{"10.0.0.3": "v1.13.5"},
		installImage: "ghcr.io/siderolabs/installer:v1.13.5",
	}
	fk := &fakeKube{nodes: []kube.NodeInfo{{Name: "w1", InternalIP: "10.0.0.3", Role: "worker", Ready: true}}}
	factory := func(context.Context, *pb.Cluster) (TalosOps, KubeOps, func(), error) {
		return ft, fk, func() {}, nil
	}
	e := NewExecutor(st, factory, t.TempDir(), time.Minute)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	job, _ := st.GetRolloutJob("home", "workers")
	if job.GetState() != pb.RolloutJobState_ROLLOUT_JOB_STATE_DONE {
		t.Fatalf("job state = %v, want DONE", job.GetState())
	}
	if len(ft.upgrades) != 1 || ft.upgrades[0].image != "ghcr.io/siderolabs/installer:v1.13.6" {
		t.Fatalf("upgrade not performed correctly: %+v", ft.upgrades)
	}
}

func TestExecutorMarksJobFailed(t *testing.T) {
	st := execStore(t)
	seedClusterPoolJob(t, st, true)

	ft := &fakeTalos{versions: map[string]string{"10.0.0.3": "v1.13.5"}, installImage: "ghcr.io/siderolabs/installer:v1.13.5"}
	fk := &fakeKube{
		nodes:    []kube.NodeInfo{{Name: "w1", InternalIP: "10.0.0.3", Role: "worker", Ready: true}},
		drainErr: map[string]error{"w1": errors.New("PDB blocked")},
	}
	factory := func(context.Context, *pb.Cluster) (TalosOps, KubeOps, func(), error) { return ft, fk, func() {}, nil }
	e := NewExecutor(st, factory, t.TempDir(), time.Minute)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	job, _ := st.GetRolloutJob("home", "workers")
	if job.GetState() != pb.RolloutJobState_ROLLOUT_JOB_STATE_FAILED {
		t.Fatalf("job state = %v, want FAILED", job.GetState())
	}
	if job.GetMessage() == "" {
		t.Fatal("failed job has no message")
	}
}
