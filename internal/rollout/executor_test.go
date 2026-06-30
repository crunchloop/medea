package rollout

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/crunchloop/medea/gen/medea/v1"
	"github.com/crunchloop/medea/internal/kube"
	"github.com/crunchloop/medea/internal/store"
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

func TestExecutorRunsK8sJob(t *testing.T) {
	st := execStore(t)
	if _, err := st.PutClusterDesired(&pb.Cluster{Name: "home", RolloutsEnabled: true}, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRolloutJob(&pb.Rollout{
		Cluster: "home", Pool: "", Kind: pb.RolloutKind_ROLLOUT_KIND_KUBERNETES,
		TargetVersion: "v1.36.2", State: pb.RolloutJobState_ROLLOUT_JOB_STATE_PENDING,
	}); err != nil {
		t.Fatal(err)
	}

	fk := &fakeKube{nodes: []kube.NodeInfo{
		{Name: "cp1", InternalIP: "10.0.0.2", Role: "controlplane", Ready: true, KubeletVersion: "v1.36.1"},
		{Name: "w1", InternalIP: "10.0.0.3", Role: "worker", Ready: true, KubeletVersion: "v1.36.1"},
	}}
	ft := &fakeTalos{versions: map[string]string{}}
	fk8 := &fakeK8s{kube: fk}
	factory := func(context.Context, *pb.Cluster) (TalosOps, KubeOps, func(), error) { return ft, fk, func() {}, nil }
	k8sFactory := func(context.Context, *pb.Cluster) (K8sOps, func(), error) { return fk8, func() {}, nil }

	e := NewExecutor(st, factory, t.TempDir(), time.Minute).WithK8sFactory(k8sFactory)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	job, _ := st.GetRolloutJob("home", "")
	if job.GetState() != pb.RolloutJobState_ROLLOUT_JOB_STATE_DONE {
		t.Fatalf("job state = %v, want DONE", job.GetState())
	}
	if len(fk8.calls) != 1 {
		t.Fatalf("upgrade-k8s not called: %v", fk8.calls)
	}
	cr, _ := st.GetClusterRollout("home")
	if cr.GetPhase() != pb.ClusterRolloutPhase_CLUSTER_ROLLOUT_PHASE_IDLE {
		t.Fatalf("cluster rollout phase = %v, want IDLE", cr.GetPhase())
	}
}

// Without a K8s upgrader wired (the default), a KUBERNETES job is refused rather
// than silently skipped — defense against a half-enabled K8s path.
func TestExecutorRefusesK8sWithoutUpgrader(t *testing.T) {
	st := execStore(t)
	if _, err := st.PutClusterDesired(&pb.Cluster{Name: "home", RolloutsEnabled: true}, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRolloutJob(&pb.Rollout{
		Cluster: "home", Kind: pb.RolloutKind_ROLLOUT_KIND_KUBERNETES,
		TargetVersion: "v1.36.2", State: pb.RolloutJobState_ROLLOUT_JOB_STATE_PENDING,
	}); err != nil {
		t.Fatal(err)
	}
	factory := func(context.Context, *pb.Cluster) (TalosOps, KubeOps, func(), error) {
		return &fakeTalos{versions: map[string]string{}}, &fakeKube{}, func() {}, nil
	}
	e := NewExecutor(st, factory, t.TempDir(), time.Minute) // no WithK8sFactory

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, _ := st.GetRolloutJob("home", "")
	if job.GetState() != pb.RolloutJobState_ROLLOUT_JOB_STATE_FAILED {
		t.Fatalf("job state = %v, want FAILED", job.GetState())
	}
}
