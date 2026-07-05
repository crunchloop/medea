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

	credsCluster string
	credsTalos   bool
	credsKube    bool
	credsSecrets bool
	credsOut     string
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
	credentials := &cobra.Command{
		Use:   "credentials",
		Short: "Fetch a cluster's stored credentials (talosconfig/kubeconfig)",
		Long: "credentials fetches a cluster's stored talosconfig/kubeconfig from Medea, so\n" +
			"you keep kubectl/talosctl access without home-cluster's _out/ (design/\n" +
			"credentials.md §5). Defaults to both client configs; --secrets is opt-in.\n\n" +
			"  medea get credentials --cluster home --kubeconfig > ~/.kube/config",
		Args: cobra.NoArgs,
		RunE: runGetCredentials,
	}

	nodepools.Flags().StringVar(&getCluster, "cluster", "", "cluster name (required)")
	machines.Flags().StringVar(&getCluster, "cluster", "", "cluster name (required)")
	machines.Flags().StringVar(&getPool, "pool", "", "filter by node pool")

	cf := credentials.Flags()
	cf.StringVar(&credsCluster, "cluster", "", "cluster name (required)")
	cf.BoolVar(&credsTalos, "talosconfig", false, "fetch talosconfig")
	cf.BoolVar(&credsKube, "kubeconfig", false, "fetch kubeconfig")
	cf.BoolVar(&credsSecrets, "secrets", false, "fetch the machine-secrets bundle (sensitive)")
	cf.StringVarP(&credsOut, "output", "o", "", "write to this file (requires selecting exactly one item)")

	getCmd.AddCommand(clusters, nodepools, machines, credentials)
	rootCmd.AddCommand(getCmd)
}

func runGetCredentials(_ *cobra.Command, _ []string) error {
	if credsCluster == "" {
		return fmt.Errorf("--cluster is required")
	}
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	resp, err := c.GetCredentials(ctx, &pb.GetCredentialsRequest{
		Cluster:     credsCluster,
		Talosconfig: credsTalos,
		Kubeconfig:  credsKube,
		Secrets:     credsSecrets,
	})
	if err != nil {
		return err
	}

	type item struct {
		name string
		data []byte
	}
	var items []item
	if len(resp.GetTalosconfig()) > 0 {
		items = append(items, item{"talosconfig", resp.GetTalosconfig()})
	}
	if len(resp.GetKubeconfig()) > 0 {
		items = append(items, item{"kubeconfig", resp.GetKubeconfig()})
	}
	if len(resp.GetSecrets()) > 0 {
		items = append(items, item{"secrets.yaml", resp.GetSecrets()})
	}
	if len(items) == 0 {
		return fmt.Errorf("no credentials returned for %q", credsCluster)
	}

	if credsOut != "" {
		if len(items) != 1 {
			return fmt.Errorf("-o requires selecting exactly one of --talosconfig/--kubeconfig/--secrets")
		}
		if err := os.WriteFile(credsOut, items[0].data, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s -> %s\n", items[0].name, credsOut)
		return nil
	}

	// A single item prints raw (so `> ~/.kube/config` works); multiple are labeled.
	if len(items) == 1 {
		_, err := os.Stdout.Write(items[0].data)
		return err
	}
	for _, it := range items {
		fmt.Printf("# --- %s ---\n", it.name)
		if _, err := os.Stdout.Write(it.data); err != nil {
			return err
		}
		fmt.Println()
	}
	return nil
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
