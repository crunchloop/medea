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

// K8sOps is the slice of the Kubernetes upgrader the reconciler drives for the
// K8s path. A narrow interface keeps the reconciler unit-testable and keeps the
// heavy, version-coupled k8supgrade import out of this package (talos-client.md
// §1, §4) — the concrete impl is wired at the composition root.
type K8sOps interface {
	UpgradeK8s(ctx context.Context, from, to string) error
}

// K8sFactory builds the per-cluster Kubernetes upgrader plus a cleanup. Injected
// (WithK8sFactory) from cmd so internal/rollout never imports the Talos main
// module.
type K8sFactory func(ctx context.Context, cl *pb.Cluster) (K8sOps, func(), error)

// Executor runs pending/in-flight Rollout jobs. It is the only thing that drives
// the reconciler, and it enforces the rollouts-enabled guard again at execution
// time (defense in depth, rollout-safety.md §6) — so even a hand-injected job
// never runs against a disabled cluster. The server only starts it when the
// operator passes --rollouts (default off), giving a third, global gate.
type Executor struct {
	store       store.Store
	factory     ClientFactory
	k8sFactory  K8sFactory
	snapshotDir string
	interval    time.Duration
}

// NewExecutor returns an Executor refreshing every interval.
func NewExecutor(st store.Store, f ClientFactory, snapshotDir string, interval time.Duration) *Executor {
	return &Executor{store: st, factory: f, snapshotDir: snapshotDir, interval: interval}
}

// WithK8sFactory injects the builder for the (heavy, quarantined) Kubernetes
// upgrader and returns the executor for chaining. Without it, the executor
// refuses KUBERNETES jobs. Wired at the composition root (cmd) so the rollout
// package never imports the Talos main module (talos-client.md §4).
func (e *Executor) WithK8sFactory(f K8sFactory) *Executor {
	e.k8sFactory = f
	return e
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

	tOps, kOps, cleanup, err := e.factory(ctx, cl)
	if err != nil {
		log.Printf("rollout executor: build clients for %q: %v", cl.GetName(), err)
		return // transient; leave the job for the next pass
	}
	if cleanup != nil {
		defer cleanup()
	}
	r := New(e.store, tOps, kOps, e.snapshotDir)

	switch job.GetKind() {
	case pb.RolloutKind_ROLLOUT_KIND_TALOS:
		job.State = pb.RolloutJobState_ROLLOUT_JOB_STATE_RUNNING
		_ = e.store.PutRolloutJob(job)
		if err := r.ReconcilePool(ctx, cl.GetName(), job.GetPool()); err != nil {
			e.finish(job, pb.RolloutJobState_ROLLOUT_JOB_STATE_FAILED, err.Error())
			return
		}
		e.finish(job, pb.RolloutJobState_ROLLOUT_JOB_STATE_DONE, "")

	case pb.RolloutKind_ROLLOUT_KIND_KUBERNETES:
		if e.k8sFactory == nil {
			e.finish(job, pb.RolloutJobState_ROLLOUT_JOB_STATE_FAILED, "kubernetes upgrader not configured")
			return
		}
		k8sOps, kCleanup, err := e.k8sFactory(ctx, cl)
		if err != nil {
			log.Printf("rollout executor: build k8s upgrader for %q: %v", cl.GetName(), err)
			return // transient; leave the job for the next pass
		}
		if kCleanup != nil {
			defer kCleanup()
		}
		job.State = pb.RolloutJobState_ROLLOUT_JOB_STATE_RUNNING
		_ = e.store.PutRolloutJob(job)
		if err := r.ReconcileK8s(ctx, cl.GetName(), job.GetTargetVersion(), k8sOps); err != nil {
			e.finish(job, pb.RolloutJobState_ROLLOUT_JOB_STATE_FAILED, err.Error())
			return
		}
		e.finish(job, pb.RolloutJobState_ROLLOUT_JOB_STATE_DONE, "")

	default:
		e.finish(job, pb.RolloutJobState_ROLLOUT_JOB_STATE_FAILED, "unknown rollout kind")
	}
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
