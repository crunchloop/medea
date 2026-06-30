// Package refresh populates the store's in-memory observed cache from the live
// cluster (datastore.md §2, §7). Observed is never persisted; the running server
// rebuilds it here — at boot and on an interval — via the talos/kube clients.
package refresh

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	pb "github.com/crunchloop/medea/gen/medea/v1"
	"github.com/crunchloop/medea/internal/creds"
	"github.com/crunchloop/medea/internal/kube"
	"github.com/crunchloop/medea/internal/store"
	"github.com/crunchloop/medea/internal/talos"
)

// kubeClient and talosClient are the slices of the wrappers refresh needs;
// narrowing to interfaces keeps the core unit-testable with fakes.
type kubeClient interface {
	ListNodes(ctx context.Context) ([]kube.NodeInfo, error)
}

type talosClient interface {
	Version(ctx context.Context, node string) (string, error)
}

// Clients bundles a cluster's clients plus a cleanup to release them.
type Clients struct {
	Kube    kubeClient
	Talos   talosClient
	Cleanup func()
}

// Factory builds clients for a cluster (real impl: CredsFactory).
type Factory func(ctx context.Context, cl *pb.Cluster) (*Clients, error)

// Refresher refreshes observed state for every cluster in the store.
type Refresher struct {
	store    store.Store
	factory  Factory
	interval time.Duration
}

// New returns a Refresher. interval is the time between full refresh passes.
func New(st store.Store, f Factory, interval time.Duration) *Refresher {
	return &Refresher{store: st, factory: f, interval: interval}
}

// Run refreshes immediately, then on the interval, until ctx is cancelled.
func (r *Refresher) Run(ctx context.Context) {
	if err := r.Once(ctx); err != nil {
		log.Printf("refresh: initial pass: %v", err)
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Once(ctx); err != nil {
				log.Printf("refresh: %v", err)
			}
		}
	}
}

// Once refreshes every cluster. A failure on one cluster does not abort the rest.
func (r *Refresher) Once(ctx context.Context) error {
	clusters, err := r.store.ListClusters()
	if err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}
	var errs []error
	for _, cl := range clusters {
		if err := r.refreshCluster(ctx, cl); err != nil {
			errs = append(errs, fmt.Errorf("cluster %q: %w", cl.GetName(), err))
		}
	}
	return errors.Join(errs...)
}

func (r *Refresher) refreshCluster(ctx context.Context, cl *pb.Cluster) error {
	clients, err := r.factory(ctx, cl)
	if err != nil {
		return fmt.Errorf("build clients: %w", err)
	}
	if clients.Cleanup != nil {
		defer clients.Cleanup()
	}

	nodes, err := clients.Kube.ListNodes(ctx)
	if err != nil {
		// Cluster unreachable: mark control plane not ready, leave the rest stale.
		r.store.SetClusterObserved(cl.GetName(), &pb.ClusterObserved{ControlPlaneReady: false})
		return fmt.Errorf("list nodes: %w", err)
	}
	byIP := make(map[string]kube.NodeInfo, len(nodes))
	for _, n := range nodes {
		byIP[n.InternalIP] = n
	}

	machines, err := r.store.ListMachines(cl.GetName(), "")
	if err != nil {
		return fmt.Errorf("list machines: %w", err)
	}
	for _, m := range machines {
		addr := m.GetTalosEndpoint()
		obs := &pb.MachineObserved{}
		if n, ok := byIP[addr]; ok {
			obs.KubernetesVersion = n.KubeletVersion
			obs.Healthy = n.Ready
			obs.Phase = phase(n.Ready)
		} else {
			obs.Phase = pb.MachinePhase_MACHINE_PHASE_NOT_READY
		}
		// Talos version (bounded; an unreachable node mid-reboot just leaves it blank).
		vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if tv, err := clients.Talos.Version(vctx, addr); err == nil {
			obs.TalosVersion = tv
		} else {
			obs.Healthy = false
		}
		cancel()
		r.store.SetMachineObserved(cl.GetName(), addr, obs)
	}

	// Cluster-level observed from the control-plane node.
	var co pb.ClusterObserved
	for _, n := range nodes {
		if n.Role == "controlplane" {
			co.KubernetesVersion = n.KubeletVersion
			co.ControlPlaneReady = n.Ready
		}
	}
	r.store.SetClusterObserved(cl.GetName(), &co)
	return nil
}

func phase(ready bool) pb.MachinePhase {
	if ready {
		return pb.MachinePhase_MACHINE_PHASE_READY
	}
	return pb.MachinePhase_MACHINE_PHASE_NOT_READY
}

// CredsFactory builds real per-cluster clients from the CredentialStore and the
// cluster's stored Talos endpoints.
func CredsFactory(cs creds.Store) Factory {
	return func(ctx context.Context, cl *pb.Cluster) (*Clients, error) {
		kb, err := cs.KubeConfig(cl.GetName())
		if err != nil {
			return nil, err
		}
		kc, err := kube.New(kb)
		if err != nil {
			return nil, err
		}
		tb, err := cs.TalosConfig(cl.GetName())
		if err != nil {
			return nil, err
		}
		tc, err := talos.New(ctx, tb, cl.GetEndpoints().GetTalos())
		if err != nil {
			return nil, err
		}
		return &Clients{Kube: kc, Talos: tc, Cleanup: func() { _ = tc.Close() }}, nil
	}
}
