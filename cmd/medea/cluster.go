package main

import (
	"fmt"
	"os"
	"strings"

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
	cf.StringVar(&ccCNI, "cni", "", `cluster CNI (cluster.network.cni.name); "" = Talos default, "none" = BYO CNI (e.g. Cilium post-bootstrap)`)
	cf.BoolVar(&ccDisableKubeProxy, "disable-kube-proxy", false, "disable kube-proxy (cluster.proxy.disabled) so the CNI takes it over")
	cf.StringArrayVar(&ccPatches, "patch", nil, "node-level gen-config patch file, @path (repeatable); NOT the CNI application")
	cf.BoolVar(&ccConfirm, "confirm", false, "arm the bootstrap (default: plan only)")

	clusterCmd.AddCommand(enable, disable, enableProv, disableProv, create)
	rootCmd.AddCommand(clusterCmd)
}

var (
	ccEndpoint, ccMac, ccIP, ccTalos, ccK8s, ccDisk, ccCNI string
	ccExtensions, ccPatches                                []string
	ccDisableKubeProxy, ccConfirm                          bool
)

func runCreateCluster(_ *cobra.Command, args []string) error {
	patches, err := readPatchFiles(ccPatches)
	if err != nil {
		return err
	}

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
		Extensions: ccExtensions, Cni: ccCNI, DisableKubeProxy: ccDisableKubeProxy,
		Patches: patches, Confirm: ccConfirm,
	})
	if err != nil {
		return err
	}
	verb := "PLAN"
	if ccConfirm {
		verb = "ARMED"
	}
	fmt.Printf("%s cluster %q: cp=%s (%s) talos=%s k8s=%s cni=%s kube-proxy=%s patches=%d phase=%s\n  %s\n",
		verb, cb.GetCluster(), cb.GetCpIp(), cb.GetCpMac(), cb.GetTalosVersion(),
		cb.GetKubernetesVersion(), cniLabel(cb.GetCni()), kubeProxyLabel(cb.GetDisableKubeProxy()),
		len(cb.GetPatches()), cb.GetPhase(), cb.GetMessage())
	if !ccConfirm {
		fmt.Println("  re-run with --confirm to arm (the server must run with --bootstrap).")
	}
	return nil
}

// readPatchFiles reads each --patch @path into raw bytes. The leading @ mirrors
// `talosctl gen config --config-patch @file`; a value without @ is rejected (we
// take files, not inline YAML, so a stray flag value can't be misread as content).
func readPatchFiles(flags []string) ([][]byte, error) {
	if len(flags) == 0 {
		return nil, nil
	}
	patches := make([][]byte, 0, len(flags))
	for _, f := range flags {
		if !strings.HasPrefix(f, "@") {
			return nil, fmt.Errorf("--patch %q must be a file reference, e.g. @path/to/patch.yaml", f)
		}
		b, err := os.ReadFile(strings.TrimPrefix(f, "@"))
		if err != nil {
			return nil, fmt.Errorf("read patch file: %w", err)
		}
		patches = append(patches, b)
	}
	return patches, nil
}

func cniLabel(cni string) string {
	if cni == "" {
		return "default"
	}
	return cni
}

func kubeProxyLabel(disabled bool) string {
	if disabled {
		return "disabled"
	}
	return "enabled"
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
