package provision

import (
	"context"
	"fmt"
	"sort"
	"time"

	pb "github.com/bilby91/medea/gen/medea/v1"
	"github.com/bilby91/medea/internal/kube"
	"github.com/bilby91/medea/internal/store"

	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
)

// drainTimeout bounds the scale-in drain before release.
const drainTimeout = 5 * time.Minute

// defaultProvisionTimeout is how long a host may sit in Provisioning (booting +
// installing + joining) before the reconciler gives up and marks it Failed.
const defaultProvisionTimeout = 20 * time.Minute

// KubeOps is the slice of the kube client the provisioning reconciler needs:
// observing which nodes have joined (scale-out) and draining a node before
// release (scale-in). Narrowed for unit-testing with a fake.
type KubeOps interface {
	ListNodes(ctx context.Context) ([]kube.NodeInfo, error)
	Drain(ctx context.Context, name string, timeout time.Duration) error
}

// SecretsFunc loads + parses a cluster's captured secrets bundle (from the
// CredentialStore, M1) — injected so the reconciler unit-tests without files.
type SecretsFunc func(cluster string) (*secrets.Bundle, error)

// Reconciler drives the scale-out provisioning flow (design/provisioning-plane.md
// §4): for a pool with replicas set, allocate an available Host, stage its boot
// config + machine config, and mark it Ready once the node joins. Scale-in /
// deprovision is v2-M4. Each pass advances one host (one op at a time).
type Reconciler struct {
	store            store.Store
	prov             Provisioner
	resolver         Resolver
	kube             KubeOps
	secretsFor       SecretsFunc
	factoryHost      string
	installDisk      string
	provisionTimeout time.Duration
}

// NewReconciler builds a provisioning reconciler. provisionTimeout <= 0 uses the
// default; it bounds how long a host may stay in Provisioning before Failed.
func NewReconciler(st store.Store, p Provisioner, r Resolver, k KubeOps, secretsFor SecretsFunc, factoryHost, installDisk string, provisionTimeout time.Duration) *Reconciler {
	if provisionTimeout <= 0 {
		provisionTimeout = defaultProvisionTimeout
	}
	return &Reconciler{
		store: st, prov: p, resolver: r, kube: k, secretsFor: secretsFor,
		factoryHost: factoryHost, installDisk: installDisk, provisionTimeout: provisionTimeout,
	}
}

// ReconcilePool converges a pool toward NodePool.replicas by provisioning
// Available hosts. It returns nil when converged, no capacity, or an op is
// already in flight; it re-checks the provisioning guard (defense in depth).
func (r *Reconciler) ReconcilePool(ctx context.Context, cluster, pool string) error {
	cl, _, err := r.store.GetCluster(cluster)
	if err != nil {
		return err
	}
	if cl == nil {
		return fmt.Errorf("provision: cluster %q not found", cluster)
	}
	if !cl.GetProvisioningEnabled() { // hard guard (provisioning-plane.md §4)
		return nil
	}
	np, _, err := r.store.GetNodePool(cluster, pool)
	if err != nil {
		return err
	}
	if np == nil {
		return fmt.Errorf("provision: nodepool %q/%q not found", cluster, pool)
	}
	if np.GetReplicas() == 0 { // v1 explicit-members mode; provisioning not engaged
		return nil
	}

	hosts, err := r.store.ListHosts(cluster, pool)
	if err != nil {
		return err
	}
	nodes, err := r.kube.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("provision: list nodes: %w", err)
	}

	// Addresses already accounted for (existing machines + bound hosts) — a
	// joined node not in this set is the one we just provisioned.
	claimed, err := r.claimedAddrs(cluster, hosts)
	if err != nil {
		return err
	}

	// 1) Advance in-flight hosts: a Provisioning host whose node has joined → Ready;
	// one that overran the timeout → Failed.
	inflight := 0
	ready := 0
	failed := 0
	for _, h := range hosts {
		switch h.GetState() {
		case pb.HostState_HOST_STATE_READY:
			ready++
		case pb.HostState_HOST_STATE_FAILED:
			failed++
		case pb.HostState_HOST_STATE_PROVISIONING:
			if n, ok := newJoinedNode(nodes, claimed, np.GetRole()); ok {
				if err := r.bind(cluster, pool, h.GetMac(), n, np.GetRole()); err != nil {
					return err
				}
				claimed[n.InternalIP] = struct{}{}
				ready++
			} else if r.timedOut(h) {
				if err := r.setHostState(cluster, h.GetMac(), pb.HostState_HOST_STATE_FAILED,
					"provision timed out: node did not join"); err != nil {
					return err
				}
				failed++
			} else {
				inflight++ // still booting/joining — park (one op at a time)
			}
		}
	}
	if inflight > 0 {
		return nil // wait for the in-flight host to join before starting another
	}
	if failed > 0 {
		// Halt: a failed provision needs operator attention (clear the host) —
		// don't burn through more hosts (mirrors rollout halt-on-failure).
		return nil
	}
	switch {
	case ready > int(np.GetReplicas()):
		// 2a) Scale in: release one Ready host (drain → unstage → remove).
		return r.scaleIn(ctx, cluster, pool, hosts, nodes)
	case ready < int(np.GetReplicas()):
		// 2b) Scale out: stage one Available host matching the selector.
		h := pickAvailable(hosts, np.GetSelector())
		if h == nil {
			return nil // no capacity — wait for a matching host to be registered
		}
		return r.stage(ctx, cl, np, h)
	default:
		return nil // converged
	}
}

// scaleIn releases one Ready host: drain its node, unstage its boot config, drop
// its Machine + pool membership, and return the host to Registered (Available
// for reuse). Deterministic victim (lowest MAC) so passes are stable.
func (r *Reconciler) scaleIn(ctx context.Context, cluster, pool string, hosts []*pb.Host, nodes []kube.NodeInfo) error {
	var victim *pb.Host
	for _, h := range hosts {
		if h.GetState() == pb.HostState_HOST_STATE_READY {
			if victim == nil || h.GetMac() < victim.GetMac() {
				victim = h
			}
		}
	}
	if victim == nil {
		return nil
	}

	if name := nodeNameFor(nodes, victim.GetAddr()); name != "" {
		if err := r.kube.Drain(ctx, name, drainTimeout); err != nil {
			return fmt.Errorf("provision: drain %s: %w", name, err)
		}
	}
	if err := r.prov.Unstage(ctx, victim.GetMac()); err != nil {
		return fmt.Errorf("provision: unstage %s: %w", victim.GetMac(), err)
	}
	if addr := victim.GetAddr(); addr != "" {
		if err := r.store.DeleteMachine(cluster, addr); err != nil {
			return err
		}
		if err := r.removeMember(cluster, pool, addr); err != nil {
			return err
		}
	}
	return r.releaseHost(cluster, victim.GetMac())
}

func (r *Reconciler) removeMember(cluster, pool, addr string) error {
	np, rev, err := r.store.GetNodePool(cluster, pool)
	if err != nil || np == nil {
		return fmt.Errorf("provision: reload nodepool %s/%s: %w", cluster, pool, err)
	}
	out := np.Members[:0]
	for _, m := range np.Members {
		if m != addr {
			out = append(out, m)
		}
	}
	np.Members = out
	_, err = r.store.PutNodePoolDesired(np, rev)
	return err
}

func (r *Reconciler) releaseHost(cluster, mac string) error {
	h, rev, err := r.store.GetHost(cluster, mac)
	if err != nil || h == nil {
		return fmt.Errorf("provision: reload host %s: %w", mac, err)
	}
	h.State = pb.HostState_HOST_STATE_REGISTERED
	h.Addr = ""
	h.Message = "released (scale-in)"
	_, err = r.store.PutHostDesired(h, rev)
	return err
}

func nodeNameFor(nodes []kube.NodeInfo, addr string) string {
	for _, n := range nodes {
		if n.InternalIP == addr {
			return n.Name
		}
	}
	return ""
}

func (r *Reconciler) stage(ctx context.Context, cl *pb.Cluster, np *pb.NodePool, h *pb.Host) error {
	version := np.GetDesired().GetTalosVersion()
	if version == "" {
		version = cl.GetDesired().GetTalosVersion()
	}
	if version == "" {
		return fmt.Errorf("provision: no Talos version for %s/%s", cl.GetName(), np.GetName())
	}
	schematic, err := r.resolver.Resolve(ctx, np.GetExtensions())
	if err != nil {
		return fmt.Errorf("provision: resolve schematic: %w", err)
	}
	bundle, err := r.secretsFor(cl.GetName())
	if err != nil {
		return fmt.Errorf("provision: load secrets: %w", err)
	}
	cfg, err := RenderWorkerConfig(WorkerConfigInput{
		ClusterName:          cl.GetName(),
		ControlPlaneEndpoint: "https://" + cl.GetEndpoints().GetKube(),
		KubernetesVersion:    cl.GetDesired().GetKubernetesVersion(),
		InstallDisk:          r.installDisk,
		InstallImage:         InstallImage(r.factoryHost, schematic, version),
		Secrets:              bundle,
	})
	if err != nil {
		return err
	}
	kernel, initrd := BootAssets(r.factoryHost, schematic, version, "")
	if err := r.prov.Stage(ctx, h.GetMac(), Profile{
		Kernel: kernel, Initrd: initrd, Args: []string{"talos.platform=metal"},
	}, cfg); err != nil {
		return fmt.Errorf("provision: stage %s: %w", h.GetMac(), err)
	}
	return r.startProvisioning(cl.GetName(), h.GetMac())
}

// startProvisioning transitions a host to Provisioning and stamps the start time
// (the deadline for the join-timeout). Time enters here at the edge of a stage.
func (r *Reconciler) startProvisioning(cluster, mac string) error {
	h, rev, err := r.store.GetHost(cluster, mac)
	if err != nil || h == nil {
		return fmt.Errorf("provision: reload host %s: %w", mac, err)
	}
	h.State = pb.HostState_HOST_STATE_PROVISIONING
	h.Message = "staged; awaiting boot + join"
	h.ProvisioningStartedAt = time.Now().UTC().Format(time.RFC3339)
	_, err = r.store.PutHostDesired(h, rev)
	return err
}

// timedOut reports whether a Provisioning host has exceeded the provision
// timeout. A missing/invalid start time is treated as not-timed-out (lenient).
func (r *Reconciler) timedOut(h *pb.Host) bool {
	ts := h.GetProvisioningStartedAt()
	if ts == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return time.Since(started) > r.provisionTimeout
}

// bind records a joined node: Host → Ready (+addr), a Machine is created, and the
// node is added to the pool's reconciler-managed membership (provisioning-plane.md §2).
func (r *Reconciler) bind(cluster, pool, mac string, n kube.NodeInfo, role pb.Role) error {
	h, hrev, err := r.store.GetHost(cluster, mac)
	if err != nil || h == nil {
		return fmt.Errorf("provision: reload host %s: %w", mac, err)
	}
	h.Addr = n.InternalIP
	h.State = pb.HostState_HOST_STATE_READY
	h.Message = ""
	if _, err := r.store.PutHostDesired(h, hrev); err != nil {
		return err
	}

	m, mrev, err := r.store.GetMachine(cluster, n.InternalIP)
	if err != nil {
		return err
	}
	if m == nil {
		m = &pb.Machine{Cluster: cluster, Pool: pool, TalosEndpoint: n.InternalIP, Role: role}
	}
	if _, err := r.store.PutMachineDesired(m, mrev); err != nil {
		return err
	}

	np, nrev, err := r.store.GetNodePool(cluster, pool)
	if err != nil || np == nil {
		return fmt.Errorf("provision: reload nodepool %s/%s: %w", cluster, pool, err)
	}
	if !contains(np.Members, n.InternalIP) {
		np.Members = append(np.Members, n.InternalIP)
		sort.Strings(np.Members)
		if _, err := r.store.PutNodePoolDesired(np, nrev); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) setHostState(cluster, mac string, st pb.HostState, msg string) error {
	h, rev, err := r.store.GetHost(cluster, mac)
	if err != nil || h == nil {
		return fmt.Errorf("provision: reload host %s: %w", mac, err)
	}
	h.State = st
	h.Message = msg
	_, err = r.store.PutHostDesired(h, rev)
	return err
}

func (r *Reconciler) claimedAddrs(cluster string, hosts []*pb.Host) (map[string]struct{}, error) {
	claimed := map[string]struct{}{}
	ms, err := r.store.ListMachines(cluster, "")
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		claimed[m.GetTalosEndpoint()] = struct{}{}
	}
	for _, h := range hosts {
		if h.GetAddr() != "" {
			claimed[h.GetAddr()] = struct{}{}
		}
	}
	return claimed, nil
}

// newJoinedNode finds a Ready node of the pool's role whose address isn't already
// claimed — i.e. the node just provisioned. Safe because the reconciler stages
// one host at a time (provisioning-plane.md §4).
func newJoinedNode(nodes []kube.NodeInfo, claimed map[string]struct{}, role pb.Role) (kube.NodeInfo, bool) {
	for _, n := range nodes {
		if !n.Ready || n.InternalIP == "" {
			continue
		}
		if _, taken := claimed[n.InternalIP]; taken {
			continue
		}
		if roleMatches(n.Role, role) {
			return n, true
		}
	}
	return kube.NodeInfo{}, false
}

func pickAvailable(hosts []*pb.Host, selector map[string]string) *pb.Host {
	for _, h := range hosts {
		if h.GetState() != pb.HostState_HOST_STATE_REGISTERED {
			continue
		}
		if matchesSelector(h.GetLabels(), selector) {
			return h
		}
	}
	return nil
}

func matchesSelector(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func roleMatches(nodeRole string, role pb.Role) bool {
	switch role {
	case pb.Role_ROLE_CONTROLPLANE:
		return nodeRole == "controlplane"
	case pb.Role_ROLE_WORKER:
		return nodeRole == "worker"
	default:
		return false
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
