package provision

import (
	"context"
	"fmt"
	"strings"
	"time"

	pb "github.com/crunchloop/medea/gen/medea/v1"
	"github.com/crunchloop/medea/internal/store"

	"gopkg.in/yaml.v3"
)

// bootstrapTimeout bounds the whole bring-up (install + reboot + etcd + apiserver)
// before the phase is marked Failed for operator attention (no BMC ⇒ no remote
// console, design/cluster-bootstrap.md §2).
const bootstrapTimeout = 30 * time.Minute

// BootstrapTalos is the slice of the Talos client the bootstrap reconciler needs.
// Built per-cluster from the generated talosconfig (BootstrapTalosFactory) so the
// reconciler unit-tests with a fake and never imports the Talos client directly.
type BootstrapTalos interface {
	Version(ctx context.Context, node string) (string, error) // reachability probe
	Bootstrap(ctx context.Context, node string) error
	Kubeconfig(ctx context.Context, node string) ([]byte, error)
}

// BootstrapTalosFactory builds a BootstrapTalos from a talosconfig + the CP node
// address (plus a cleanup). Injected at the composition root.
type BootstrapTalosFactory func(talosconfig []byte, node string) (BootstrapTalos, func(), error)

// BootstrapCreds is the credential-store slice the reconciler reads/writes.
type BootstrapCreds interface {
	Secrets(cluster string) ([]byte, error)
	PutSecrets(cluster string, b []byte) error
	TalosConfig(cluster string) ([]byte, error)
	Put(cluster string, talos, kube []byte) error
}

// BootstrapReconciler drives Medea-driven creation of a new single-control-plane
// cluster through the ClusterBootstrap phase machine (design/cluster-bootstrap.md
// §2). Reconciler-owned (LWW); advances at most one phase per call; resumable
// across the CP node's install-reboot (the phase is persisted, so a Medea restart
// resumes past a completed step). Gated by the executor (--bootstrap) plus the
// record's existence (created only by a confirmed `cluster create`).
type BootstrapReconciler struct {
	store       store.Store
	prov        Provisioner
	resolver    Resolver
	creds       BootstrapCreds
	talosFor    BootstrapTalosFactory
	factoryHost string
	timeout     time.Duration
}

// NewBootstrapReconciler builds the reconciler. timeout <= 0 uses the default.
func NewBootstrapReconciler(st store.Store, p Provisioner, r Resolver, c BootstrapCreds, talosFor BootstrapTalosFactory, factoryHost string, timeout time.Duration) *BootstrapReconciler {
	if timeout <= 0 {
		timeout = bootstrapTimeout
	}
	return &BootstrapReconciler{store: st, prov: p, resolver: r, creds: c, talosFor: talosFor, factoryHost: factoryHost, timeout: timeout}
}

// Reconcile advances one cluster's bootstrap by at most one phase. Terminal
// phases (Ready/Failed) are no-ops. A returned nil covers both a normal step and
// an expected wait (park-and-retry, e.g. the node still installing); only a
// genuine, unrecoverable store/render error bubbles up.
func (r *BootstrapReconciler) Reconcile(ctx context.Context, cluster string) error {
	cb, err := r.store.GetClusterBootstrap(cluster)
	if err != nil {
		return err
	}
	if cb == nil {
		return nil
	}

	switch cb.GetPhase() {
	case pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_READY,
		pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_FAILED:
		return nil
	}

	// Whole-bringup timeout (started_at is stamped at the first transition).
	if r.timedOut(cb) {
		return r.put(cb, pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_FAILED, "bootstrap timed out")
	}

	switch cb.GetPhase() {
	case pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_NOT_BOOTSTRAPPED:
		cb.StartedAt = time.Now().UTC().Format(time.RFC3339)
		return r.put(cb, pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_GENERATING_SECRETS, "starting bootstrap")

	case pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_GENERATING_SECRETS:
		return r.generateSecrets(cb)

	case pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_STAGING:
		return r.stage(ctx, cb)

	case pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_AWAITING_INSTALL:
		return r.awaitInstall(ctx, cb)

	case pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_BOOTSTRAPPING:
		return r.bootstrapEtcd(ctx, cb)

	case pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_AWAITING_HEALTHY,
		pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_FETCHING_KUBECONFIG:
		return r.fetchKubeconfig(ctx, cb)
	}
	return nil
}

// generateSecrets mints the cluster PKI (once) into the CredentialStore.
func (r *BootstrapReconciler) generateSecrets(cb *pb.ClusterBootstrap) error {
	// secrets-once: never regenerate for a cluster that already has a bundle
	// (that would mint a *different* cluster the staged config wouldn't match).
	if b, err := r.creds.Secrets(cb.GetCluster()); err == nil && len(b) > 0 {
		return r.put(cb, pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_STAGING, "secrets already present")
	}
	bundle, err := GenerateSecrets()
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("provision: marshal secrets: %w", err)
	}
	if err := r.creds.PutSecrets(cb.GetCluster(), out); err != nil {
		return err
	}
	return r.put(cb, pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_STAGING, "generated cluster secrets")
}

// stage renders the CP machine config + admin talosconfig from the bundle, stores
// the talosconfig (so we can reach the node), and stages the boot to Matchbox.
func (r *BootstrapReconciler) stage(ctx context.Context, cb *pb.ClusterBootstrap) error {
	raw, err := r.creds.Secrets(cb.GetCluster())
	if err != nil {
		return fmt.Errorf("provision: load secrets: %w", err)
	}
	bundle, err := LoadSecretsBundle(raw)
	if err != nil {
		return err
	}

	schematic, err := r.resolver.Resolve(ctx, cb.GetExtensions())
	if err != nil {
		return fmt.Errorf("provision: resolve schematic: %w", err)
	}

	cfg, err := RenderControlPlaneConfig(ControlPlaneConfigInput{
		ClusterName:                    cb.GetCluster(),
		ControlPlaneEndpoint:           cb.GetCpEndpoint(),
		KubernetesVersion:              cb.GetKubernetesVersion(),
		InstallDisk:                    cb.GetInstallDisk(),
		InstallImage:                   InstallImage(r.factoryHost, schematic, cb.GetTalosVersion()),
		Secrets:                        bundle,
		AllowSchedulingOnControlPlanes: true, // single-node homelab CP runs workloads
		CNI:                            cb.GetCni(),
		DisableKubeProxy:               cb.GetDisableKubeProxy(),
		Patches:                        cb.GetPatches(),
	})
	if err != nil {
		return err
	}

	tc, err := Talosconfig(cb.GetCluster(), cb.GetCpEndpoint(), cb.GetKubernetesVersion(), bundle)
	if err != nil {
		return err
	}
	// Store the talosconfig now (kubeconfig only exists post-bootstrap).
	if err := r.creds.Put(cb.GetCluster(), tc, nil); err != nil {
		return fmt.Errorf("provision: store talosconfig: %w", err)
	}

	kernel, initrd := BootAssets(r.factoryHost, schematic, cb.GetTalosVersion(), "")
	if err := r.prov.Stage(ctx, cb.GetCpMac(), Profile{
		Kernel: kernel, Initrd: initrd, Args: []string{"talos.platform=metal"},
	}, cfg); err != nil {
		return fmt.Errorf("provision: stage control-plane: %w", err)
	}
	return r.put(cb, pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_AWAITING_INSTALL, "staged; awaiting install + boot")
}

// awaitInstall parks until the freshly-installed node's Talos API is reachable.
func (r *BootstrapReconciler) awaitInstall(ctx context.Context, cb *pb.ClusterBootstrap) error {
	tc, cleanup, err := r.talos(cb)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := tc.Version(ctx, cb.GetCpIp()); err != nil {
		return nil // park: node still installing / rebooting (expected)
	}
	return r.put(cb, pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_BOOTSTRAPPING, "node reachable; bootstrapping etcd")
}

// bootstrapEtcd initializes etcd exactly once. Retrying is safe: Talos refuses to
// re-init an already-bootstrapped node (it errors rather than wiping), so an
// "already exists" error is treated as success.
func (r *BootstrapReconciler) bootstrapEtcd(ctx context.Context, cb *pb.ClusterBootstrap) error {
	tc, cleanup, err := r.talos(cb)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := tc.Bootstrap(ctx, cb.GetCpIp()); err != nil {
		if alreadyBootstrapped(err) {
			return r.put(cb, pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_AWAITING_HEALTHY, "etcd already bootstrapped")
		}
		return nil // park: transient (apiserver/etcd not ready) — retry
	}
	return r.put(cb, pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_AWAITING_HEALTHY, "etcd bootstrapped")
}

// fetchKubeconfig waits for the apiserver (by fetching the kubeconfig, which only
// succeeds once it's up), stores it, seeds inventory, and marks the cluster Ready.
func (r *BootstrapReconciler) fetchKubeconfig(ctx context.Context, cb *pb.ClusterBootstrap) error {
	tc, cleanup, err := r.talos(cb)
	if err != nil {
		return err
	}
	defer cleanup()
	kube, err := tc.Kubeconfig(ctx, cb.GetCpIp())
	if err != nil {
		return nil // park: apiserver not up yet
	}
	talosBytes, err := r.creds.TalosConfig(cb.GetCluster())
	if err != nil {
		return err
	}
	if err := r.creds.Put(cb.GetCluster(), talosBytes, kube); err != nil {
		return fmt.Errorf("provision: store kubeconfig: %w", err)
	}
	if err := r.seedInventory(cb); err != nil {
		return err
	}
	return r.put(cb, pb.ClusterBootstrapPhase_CLUSTER_BOOTSTRAP_PHASE_READY, "cluster bootstrapped")
}

// seedInventory records the new cluster so Medea operates it: a Cluster desired
// record, a control-plane NodePool, and the CP Machine.
func (r *BootstrapReconciler) seedInventory(cb *pb.ClusterBootstrap) error {
	cluster := cb.GetCluster()
	kubeEndpoint := strings.TrimPrefix(cb.GetCpEndpoint(), "https://")

	if cl, _, _ := r.store.GetCluster(cluster); cl == nil {
		if _, err := r.store.PutClusterDesired(&pb.Cluster{
			Name:      cluster,
			Desired:   &pb.ClusterDesired{TalosVersion: cb.GetTalosVersion(), KubernetesVersion: cb.GetKubernetesVersion()},
			Endpoints: &pb.ClusterEndpoints{Talos: []string{cb.GetCpIp()}, Kube: kubeEndpoint},
		}, 0); err != nil {
			return err
		}
	}
	if np, _, _ := r.store.GetNodePool(cluster, "controlplane"); np == nil {
		if _, err := r.store.PutNodePoolDesired(&pb.NodePool{
			Cluster: cluster, Name: "controlplane", Role: pb.Role_ROLE_CONTROLPLANE,
			Members: []string{cb.GetCpIp()},
		}, 0); err != nil {
			return err
		}
	}
	if m, _, _ := r.store.GetMachine(cluster, cb.GetCpIp()); m == nil {
		if _, err := r.store.PutMachineDesired(&pb.Machine{
			Cluster: cluster, Pool: "controlplane", TalosEndpoint: cb.GetCpIp(), Role: pb.Role_ROLE_CONTROLPLANE,
		}, 0); err != nil {
			return err
		}
	}
	return nil
}

func (r *BootstrapReconciler) talos(cb *pb.ClusterBootstrap) (BootstrapTalos, func(), error) {
	tc, err := r.creds.TalosConfig(cb.GetCluster())
	if err != nil {
		return nil, nil, fmt.Errorf("provision: read talosconfig: %w", err)
	}
	return r.talosFor(tc, cb.GetCpIp())
}

// put stamps phase + message and persists (LWW). Time enters at the reconciler edge.
func (r *BootstrapReconciler) put(cb *pb.ClusterBootstrap, phase pb.ClusterBootstrapPhase, msg string) error {
	cb.Phase = phase
	cb.Message = msg
	return r.store.PutClusterBootstrap(cb)
}

func (r *BootstrapReconciler) timedOut(cb *pb.ClusterBootstrap) bool {
	ts := cb.GetStartedAt()
	if ts == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return time.Since(started) > r.timeout
}

func alreadyBootstrapped(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already") || strings.Contains(s, "not empty")
}
