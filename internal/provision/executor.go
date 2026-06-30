package provision

import (
	"context"
	"log"
	"time"

	pb "github.com/crunchloop/medea/gen/medea/v1"
	"github.com/crunchloop/medea/internal/creds"
	"github.com/crunchloop/medea/internal/kube"
	"github.com/crunchloop/medea/internal/store"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
)

// KubeFactory builds a per-cluster KubeOps (+cleanup) for the executor. Injected
// so the executor unit-tests with fakes.
type KubeFactory func(ctx context.Context, cl *pb.Cluster) (KubeOps, func(), error)

// Executor runs the provisioning reconciler over every provisioning-enabled
// cluster/pool on an interval (mirrors rollout.Executor). The Matchbox driver
// and Image-Factory resolver are process-wide; the kube client and secrets are
// per-cluster. It re-checks the provisioning guard (defense in depth) and is
// started by `medea serve` only when --provisioning is set (a global gate).
type Executor struct {
	store            store.Store
	prov             Provisioner
	resolver         Resolver
	kubeFor          KubeFactory
	secretsFor       SecretsFunc
	factoryHost      string
	installDisk      string
	provisionTimeout time.Duration
	interval         time.Duration
}

// NewExecutor builds a provisioning executor. provisionTimeout bounds how long a
// host may sit in Provisioning before Failed (<= 0 = reconciler default).
func NewExecutor(st store.Store, prov Provisioner, resolver Resolver, kubeFor KubeFactory, secretsFor SecretsFunc, factoryHost, installDisk string, provisionTimeout, interval time.Duration) *Executor {
	return &Executor{
		store: st, prov: prov, resolver: resolver, kubeFor: kubeFor, secretsFor: secretsFor,
		factoryHost: factoryHost, installDisk: installDisk, provisionTimeout: provisionTimeout, interval: interval,
	}
}

// Run reconciles immediately, then on the interval, until ctx is cancelled.
func (e *Executor) Run(ctx context.Context) {
	if err := e.RunOnce(ctx); err != nil {
		log.Printf("provision executor: %v", err)
	}
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.RunOnce(ctx); err != nil {
				log.Printf("provision executor: %v", err)
			}
		}
	}
}

// RunOnce reconciles every provisioning-enabled cluster's replica-managed pools.
func (e *Executor) RunOnce(ctx context.Context) error {
	clusters, err := e.store.ListClusters()
	if err != nil {
		return err
	}
	for _, cl := range clusters {
		if !cl.GetProvisioningEnabled() {
			continue // hard guard, re-checked here
		}
		pools, err := e.store.ListNodePools(cl.GetName())
		if err != nil {
			return err
		}
		for _, np := range pools {
			if np.GetReplicas() == 0 {
				continue // explicit-members mode; not provisioning-managed
			}
			kc, cleanup, err := e.kubeFor(ctx, cl)
			if err != nil {
				log.Printf("provision executor: build kube for %q: %v", cl.GetName(), err)
				continue // transient; next pass
			}
			r := NewReconciler(e.store, e.prov, e.resolver, kc, e.secretsFor, e.factoryHost, e.installDisk, e.provisionTimeout)
			if err := r.ReconcilePool(ctx, cl.GetName(), np.GetName()); err != nil {
				log.Printf("provision executor: reconcile %s/%s: %v", cl.GetName(), np.GetName(), err)
			}
			if cleanup != nil {
				cleanup()
			}
		}
	}
	return nil
}

// CredsKubeFactory builds per-cluster kube clients from the CredentialStore.
func CredsKubeFactory(cs creds.Store) KubeFactory {
	return func(_ context.Context, cl *pb.Cluster) (KubeOps, func(), error) {
		kb, err := cs.KubeConfig(cl.GetName())
		if err != nil {
			return nil, nil, err
		}
		kc, err := kube.New(kb)
		if err != nil {
			return nil, nil, err
		}
		return kc, func() {}, nil
	}
}

// CredsSecretsFunc loads a cluster's captured secrets bundle from the
// CredentialStore (secrets.yaml, M1).
func CredsSecretsFunc(cs creds.Store) SecretsFunc {
	return func(cluster string) (*secrets.Bundle, error) {
		b, err := cs.Secrets(cluster)
		if err != nil {
			return nil, err
		}
		return LoadSecretsBundle(b)
	}
}
