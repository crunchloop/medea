package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	pb "github.com/crunchloop/medea/gen/medea/v1"
)

var (
	getCluster string
	getPool    string
)

func init() {
	getCmd := &cobra.Command{Use: "get", Short: "Read cluster state"}

	clusters := &cobra.Command{
		Use:   "clusters",
		Short: "List clusters",
		Args:  cobra.NoArgs,
		RunE:  runGetClusters,
	}
	nodepools := &cobra.Command{
		Use:   "nodepools",
		Short: "List node pools in a cluster",
		Args:  cobra.NoArgs,
		RunE:  runGetNodePools,
	}
	machines := &cobra.Command{
		Use:   "machines",
		Short: "List machines in a cluster",
		Args:  cobra.NoArgs,
		RunE:  runGetMachines,
	}
	nodepools.Flags().StringVar(&getCluster, "cluster", "", "cluster name (required)")
	machines.Flags().StringVar(&getCluster, "cluster", "", "cluster name (required)")
	machines.Flags().StringVar(&getPool, "pool", "", "filter by node pool")

	getCmd.AddCommand(clusters, nodepools, machines)
	rootCmd.AddCommand(getCmd)
}

func runGetClusters(_ *cobra.Command, _ []string) error {
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	resp, err := c.ListClusters(ctx, &pb.ListClustersRequest{})
	if err != nil {
		return err
	}
	tw := newTab()
	fmt.Fprintln(tw, "NAME\tTALOS\tK8S(DESIRED)\tK8S(OBSERVED)\tCP-READY")
	for _, cl := range resp.GetClusters() {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\n",
			cl.GetName(),
			dash(cl.GetDesired().GetTalosVersion()),
			dash(cl.GetDesired().GetKubernetesVersion()),
			dash(cl.GetObserved().GetKubernetesVersion()),
			cl.GetObserved().GetControlPlaneReady(),
		)
	}
	return tw.Flush()
}

func runGetNodePools(_ *cobra.Command, _ []string) error {
	if getCluster == "" {
		return errClusterRequired
	}
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	resp, err := c.ListNodePools(ctx, &pb.ListNodePoolsRequest{Cluster: getCluster})
	if err != nil {
		return err
	}
	tw := newTab()
	fmt.Fprintln(tw, "NAME\tROLE\tMEMBERS\tTALOS\tPAUSED")
	for _, np := range resp.GetNodePools() {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%t\n",
			np.GetName(), roleStr(np.GetRole()), len(np.GetMembers()),
			dash(np.GetDesired().GetTalosVersion()), np.GetPaused())
	}
	return tw.Flush()
}

func runGetMachines(_ *cobra.Command, _ []string) error {
	if getCluster == "" {
		return errClusterRequired
	}
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	resp, err := c.ListMachines(ctx, &pb.ListMachinesRequest{Cluster: getCluster, Pool: getPool})
	if err != nil {
		return err
	}
	tw := newTab()
	fmt.Fprintln(tw, "ENDPOINT\tPOOL\tROLE\tPHASE\tTALOS\tK8S\tHEALTHY")
	for _, m := range resp.GetMachines() {
		o := m.GetObserved()
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%t\n",
			m.GetTalosEndpoint(), dash(m.GetPool()), roleStr(m.GetRole()),
			phaseStr(o.GetPhase()), dash(o.GetTalosVersion()), dash(o.GetKubernetesVersion()), o.GetHealthy())
	}
	return tw.Flush()
}

func newTab() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func roleStr(r pb.Role) string {
	switch r {
	case pb.Role_ROLE_CONTROLPLANE:
		return "controlplane"
	case pb.Role_ROLE_WORKER:
		return "worker"
	default:
		return "-"
	}
}

func phaseStr(p pb.MachinePhase) string {
	return dash(strings.ToLower(strings.TrimPrefix(p.String(), "MACHINE_PHASE_")))
}
