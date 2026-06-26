package rollout

import (
	"context"
	"log"
	"time"

	pb "github.com/bilby91/medea/gen/medea/v1"
	"github.com/bilby91/medea/internal/creds"
	"github.com/bilby91/medea/internal/kube"
	"github.com/bilby91/medea/internal/store"
	"github.com/bilby91/medea/internal/talos"
)

// ClientFactory builds the per-cluster talos/kube clients a job needs, plus a
// cleanup. Injected so the executor unit-tests with fakes.
type ClientFactory func(ctx context.Context, cl *pb.Cluster) (TalosOps, KubeOps, func(), error)

// Executor runs pending/in-flight Rollout jobs. It is the only thing that drives
// the reconciler, and it enforces the rollouts-enabled guard again at execution
// time (defense in depth, rollout-safety.md §6) — so even a hand-injected job
// never runs against a disabled cluster. The server only starts it when the
// operator passes --rollouts (default off), giving a third, global gate.
type Executor struct {
	store       store.Store
	factory     ClientFactory
	snapshotDir string
	interval    time.Duration
}

// NewExecutor returns an Executor refreshing every interval.
func NewExecutor(st store.Store, f ClientFactory, snapshotDir string, interval time.Duration) *Executor {
	return &Executor{store: st, factory: f, snapshotDir: snapshotDir, interval: interval}
}

// Run resumes/executes jobs immediately (boot resume), then on the interval,
// until ctx is cancelled.
func (e *Executor) Run(ctx context.Context) {
	if err := e.RunOnce(ctx); err != nil {
		log.Printf("rollout executor: %v", err)
	}
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.RunOnce(ctx); err != nil {
				log.Printf("rollout executor: %v", err)
			}
		}
	}
}

// RunOnce executes every actionable job on every enabled cluster. Boot resume is
// implicit: a job left RUNNING is re-driven (ReconcilePool is idempotent).
func (e *Executor) RunOnce(ctx context.Context) error {
	clusters, err := e.store.ListClusters()
	if err != nil {
		return err
	}
	for _, cl := range clusters {
		if !cl.GetRolloutsEnabled() {
			continue // hard guard, re-checked here
		}
		jobs, err := e.store.ListRolloutJobs(cl.GetName())
		if err != nil {
			return err
		}
		for _, job := range jobs {
			if !actionable(job.GetState()) {
				continue
			}
			e.runJob(ctx, cl, job)
		}
	}
	return nil
}

func (e *Executor) runJob(ctx context.Context, cl *pb.Cluster, job *pb.Rollout) {
	// Re-check the guard right before acting — never trust a stored job alone.
	if !cl.GetRolloutsEnabled() {
		return
	}
	if job.GetKind() != pb.RolloutKind_ROLLOUT_KIND_TALOS {
		e.finish(job, pb.RolloutJobState_ROLLOUT_JOB_STATE_FAILED, "kubernetes rollouts not supported in v1")
		return
	}

	tOps, kOps, cleanup, err := e.factory(ctx, cl)
	if err != nil {
		log.Printf("rollout executor: build clients for %q: %v", cl.GetName(), err)
		return // transient; leave the job for the next pass
	}
	if cleanup != nil {
		defer cleanup()
	}

	job.State = pb.RolloutJobState_ROLLOUT_JOB_STATE_RUNNING
	_ = e.store.PutRolloutJob(job)

	r := New(e.store, tOps, kOps, e.snapshotDir)
	if err := r.ReconcilePool(ctx, cl.GetName(), job.GetPool()); err != nil {
		e.finish(job, pb.RolloutJobState_ROLLOUT_JOB_STATE_FAILED, err.Error())
		return
	}
	e.finish(job, pb.RolloutJobState_ROLLOUT_JOB_STATE_DONE, "")
}

func (e *Executor) finish(job *pb.Rollout, state pb.RolloutJobState, msg string) {
	job.State = state
	job.Message = msg
	_ = e.store.PutRolloutJob(job)
}

func actionable(s pb.RolloutJobState) bool {
	return s == pb.RolloutJobState_ROLLOUT_JOB_STATE_PENDING ||
		s == pb.RolloutJobState_ROLLOUT_JOB_STATE_RUNNING
}

// CredsFactory builds real per-cluster clients from the CredentialStore and the
// cluster's stored Talos endpoints.
func CredsFactory(cs creds.Store) ClientFactory {
	return func(ctx context.Context, cl *pb.Cluster) (TalosOps, KubeOps, func(), error) {
		kb, err := cs.KubeConfig(cl.GetName())
		if err != nil {
			return nil, nil, nil, err
		}
		kc, err := kube.New(kb)
		if err != nil {
			return nil, nil, nil, err
		}
		tb, err := cs.TalosConfig(cl.GetName())
		if err != nil {
			return nil, nil, nil, err
		}
		tc, err := talos.New(ctx, tb, cl.GetEndpoints().GetTalos())
		if err != nil {
			return nil, nil, nil, err
		}
		return tc, kc, func() { _ = tc.Close() }, nil
	}
}
