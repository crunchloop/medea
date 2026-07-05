package main

import (
	"fmt"

	"github.com/spf13/cobra"

	pb "github.com/crunchloop/medea/gen/medea/v1"
)

func init() {
	clusterCmd := &cobra.Command{Use: "cluster", Short: "Cluster administration"}

	enable := &cobra.Command{
		Use:   "enable-rollouts <cluster>",
		Short: "Allow rollouts on a cluster (deliberate; off by default)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return setRollouts(args[0], true) },
	}
	disable := &cobra.Command{
		Use:   "disable-rollouts <cluster>",
		Short: "Disallow rollouts on a cluster",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return setRollouts(args[0], false) },
	}
	enableProv := &cobra.Command{
		Use:   "enable-provisioning <cluster>",
		Short: "Allow provisioning on a cluster (deliberate; off by default)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return setProvisioning(args[0], true) },
	}
	disableProv := &cobra.Command{
		Use:   "disable-provisioning <cluster>",
		Short: "Disallow provisioning on a cluster",
		Args:  cobra.ExactArgs(1),
		RunE:  func(_ *cobra.Command, args []string) error { return setProvisioning(args[0], false) },
	}
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a NEW cluster, Medea-driven (plan by default; --confirm to arm)",
		Args:  cobra.ExactArgs(1),
		RunE:  runCreateCluster,
	}
	cf := create.Flags()
	cf.StringVar(&ccEndpoint, "cp-endpoint", "", "control-plane endpoint, e.g. https://192.168.14.160:6443 (required)")
	cf.StringVar(&ccMac, "cp-mac", "", "control-plane host NIC MAC (required)")
	cf.StringVar(&ccIP, "cp-ip", "", "pinned control-plane IP (required)")
	cf.StringVar(&ccTalos, "talos-version", "", "Talos version, e.g. v1.13.5 (required)")
	cf.StringVar(&ccK8s, "kubernetes-version", "", "Kubernetes version, e.g. v1.36.1 (required)")
	cf.StringVar(&ccDisk, "install-disk", "/dev/nvme0n1", "install disk")
	cf.StringSliceVar(&ccExtensions, "extensions", nil, "Talos system extensions (schematic set)")
	cf.BoolVar(&ccConfirm, "confirm", false, "arm the bootstrap (default: plan only)")

	clusterCmd.AddCommand(enable, disable, enableProv, disableProv, create)
	rootCmd.AddCommand(clusterCmd)
}

var (
	ccEndpoint, ccMac, ccIP, ccTalos, ccK8s, ccDisk string
	ccExtensions                                    []string
	ccConfirm                                       bool
)

func runCreateCluster(_ *cobra.Command, args []string) error {
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	cb, err := c.CreateCluster(ctx, &pb.CreateClusterRequest{
		Name: args[0], CpEndpoint: ccEndpoint, CpMac: ccMac, CpIp: ccIP,
		TalosVersion: ccTalos, KubernetesVersion: ccK8s, InstallDisk: ccDisk,
		Extensions: ccExtensions, Confirm: ccConfirm,
	})
	if err != nil {
		return err
	}
	verb := "PLAN"
	if ccConfirm {
		verb = "ARMED"
	}
	fmt.Printf("%s cluster %q: cp=%s (%s) talos=%s k8s=%s phase=%s\n  %s\n",
		verb, cb.GetCluster(), cb.GetCpIp(), cb.GetCpMac(), cb.GetTalosVersion(),
		cb.GetKubernetesVersion(), cb.GetPhase(), cb.GetMessage())
	if !ccConfirm {
		fmt.Println("  re-run with --confirm to arm (the server must run with --bootstrap).")
	}
	return nil
}

func setRollouts(cluster string, enable bool) error {
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	var cl *pb.Cluster
	if enable {
		cl, err = c.EnableRollouts(ctx, &pb.EnableRolloutsRequest{Cluster: cluster})
	} else {
		cl, err = c.DisableRollouts(ctx, &pb.EnableRolloutsRequest{Cluster: cluster})
	}
	if err != nil {
		return err
	}
	fmt.Printf("cluster %q: rollouts_enabled=%t\n", cl.GetName(), cl.GetRolloutsEnabled())
	return nil
}

func setProvisioning(cluster string, enable bool) error {
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	var cl *pb.Cluster
	if enable {
		cl, err = c.EnableProvisioning(ctx, &pb.EnableProvisioningRequest{Cluster: cluster})
	} else {
		cl, err = c.DisableProvisioning(ctx, &pb.EnableProvisioningRequest{Cluster: cluster})
	}
	if err != nil {
		return err
	}
	fmt.Printf("cluster %q: provisioning_enabled=%t\n", cl.GetName(), cl.GetProvisioningEnabled())
	return nil
}
