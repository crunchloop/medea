// Package seed populates the store with a cluster's desired + identity state
// read from the live cluster (the M1 state-seeding step). It is the read path of
// talos-client.md run once: kube node list + per-node Talos version → store.
//
// Seeding sets desired = current reality, so it never triggers a rollout on
// import. Observed (health/versions at runtime) is not written here — it is the
// in-memory cache the server rebuilds on boot (datastore.md §2, §7).
package seed

import (
	"fmt"
	"sort"

	pb "github.com/crunchloop/medea/gen/medea/v1"
	"github.com/crunchloop/medea/internal/kube"
	"github.com/crunchloop/medea/internal/store"
)

// Inputs is everything seeding needs, with the Talos lookup injected so the
// assembly logic is unit-testable without a real cluster.
type Inputs struct {
	Cluster        string
	KubeEndpoint   string
	TalosEndpoints []string
	Nodes          []kube.NodeInfo
	// TalosVersion returns the Talos version tag for a node's address.
	TalosVersion func(addr string) (string, error)
}

// Result summarizes what was written.
type Result struct {
	Cluster   string
	Pools     []string
	Machines  int
	TalosSeed string
	K8sSeed   string
}

// Apply writes the Cluster, NodePools, and Machines derived from in into st.
// It is idempotent: re-seeding updates existing records via CAS.
func Apply(st store.Store, in Inputs) (Result, error) {
	if in.Cluster == "" {
		return Result{}, fmt.Errorf("seed: cluster name required")
	}

	type machine struct {
		addr, pool string
		role       pb.Role
	}
	var (
		machines    []machine
		poolMembers = map[string][]string{}
		poolRole    = map[string]pb.Role{}
		clusterTalos, clusterK8s string
	)

	for _, n := range in.Nodes {
		addr := n.InternalIP
		if addr == "" {
			return Result{}, fmt.Errorf("seed: node %q has no internal IP", n.Name)
		}
		pool := poolName(n.Role)
		r := pbRole(n.Role)
		tv, err := in.TalosVersion(addr)
		if err != nil {
			return Result{}, fmt.Errorf("seed: talos version for %s (%s): %w", n.Name, addr, err)
		}

		machines = append(machines, machine{addr: addr, pool: pool, role: r})
		poolMembers[pool] = append(poolMembers[pool], addr)
		poolRole[pool] = r

		// Cluster-wide defaults come from a control-plane node.
		if n.Role == "controlplane" {
			clusterTalos, clusterK8s = tv, n.KubeletVersion
		}
	}
	if len(machines) == 0 {
		return Result{}, fmt.Errorf("seed: no nodes found")
	}
	if clusterTalos == "" {
		// No control-plane node seen; fall back to the first node.
		clusterTalos, _ = in.TalosVersion(machines[0].addr)
		clusterK8s = in.Nodes[0].KubeletVersion
	}

	// Cluster (desired = current reality).
	cl := &pb.Cluster{
		Name:      in.Cluster,
		Desired:   &pb.ClusterDesired{TalosVersion: clusterTalos, KubernetesVersion: clusterK8s},
		Endpoints: &pb.ClusterEndpoints{Talos: in.TalosEndpoints, Kube: in.KubeEndpoint},
	}
	if _, rev, err := st.GetCluster(in.Cluster); err != nil {
		return Result{}, err
	} else if _, err := st.PutClusterDesired(cl, rev); err != nil {
		return Result{}, err
	}

	// NodePools (sorted for deterministic output).
	pools := make([]string, 0, len(poolMembers))
	for p := range poolMembers {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	for _, pool := range pools {
		members := poolMembers[pool]
		sort.Strings(members)
		np := &pb.NodePool{
			Cluster: in.Cluster, Name: pool, Role: poolRole[pool],
			Members: members, Desired: &pb.NodePoolDesired{}, // "" = inherit cluster
		}
		_, rev, err := st.GetNodePool(in.Cluster, pool)
		if err != nil {
			return Result{}, err
		}
		if _, err := st.PutNodePoolDesired(np, rev); err != nil {
			return Result{}, err
		}
	}

	// Machines.
	for _, m := range machines {
		mc := &pb.Machine{Cluster: in.Cluster, Pool: m.pool, TalosEndpoint: m.addr, Role: m.role}
		_, rev, err := st.GetMachine(in.Cluster, m.addr)
		if err != nil {
			return Result{}, err
		}
		if _, err := st.PutMachineDesired(mc, rev); err != nil {
			return Result{}, err
		}
	}

	return Result{
		Cluster: in.Cluster, Pools: pools, Machines: len(machines),
		TalosSeed: clusterTalos, K8sSeed: clusterK8s,
	}, nil
}

func poolName(role string) string {
	if role == "controlplane" {
		return "controlplane"
	}
	return "workers"
}

func pbRole(role string) pb.Role {
	if role == "controlplane" {
		return pb.Role_ROLE_CONTROLPLANE
	}
	return pb.Role_ROLE_WORKER
}
