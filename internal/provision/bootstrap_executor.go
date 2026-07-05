package provision

import (
	"context"
	"log"
	"time"

	pb "github.com/crunchloop/medea/gen/medea/v1"
	"github.com/crunchloop/medea/internal/store"
)

// BootstrapExecutor drives non-terminal ClusterBootstrap records through the
// BootstrapReconciler on an interval (design/cluster-bootstrap.md §7). Started
// only when the operator passes `serve --bootstrap` (the global gate); a
// ClusterBootstrap record exists only after a confirmed `medea cluster create`.
type BootstrapExecutor struct {
	store      store.Store
	reconciler *BootstrapReconciler
	interval   time.Duration
}

// NewBootstrapExecutor returns an executor reconciling every interval.
func NewBootstrapExecutor(st store.Store, r *BootstrapReconciler, interval time.Duration) *BootstrapExecutor {
	return &BootstrapExecutor{store: st, reconciler: r, interval: interval}
}

// Run reconciles immediately (boot resume — a bootstrap survives a Medea restart),
// then on the interval until ctx is cancelled.
func (e *BootstrapExecutor) Run(ctx context.Context) {
	if err := e.RunOnce(ctx); err != nil {
		log.Printf("bootstrap executor: %v", err)
	}
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.RunOnce(ctx); err != nil {
				log.Printf("bootstrap executor: %v", err)
			}
		}
	}
}

// RunOnce advances every non-terminal ClusterBootstrap by one phase.
func (e *BootstrapExecutor) RunOnce(ctx context.Context) error {
	bootstraps, err := e.store.ListClusterBootstraps()
	if err != nil {
		return err
	}
	for _, cb := range bootstraps {
		if terminalBootstrap(cb.GetPhase()) {
			continue
		}
		if err := e.reconciler.Reconcile(ctx, cb.GetCluster()); err != nil {
			log.Printf("bootstrap %q: %v", cb.GetCluster(), err)
		}
	}
	return nil
}

func terminalBootstrap(p pb.ClusterBootstrapPhase) bool {
	return p == pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_READY ||
		p == pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_FAILED
}
