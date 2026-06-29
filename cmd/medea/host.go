package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	pb "github.com/bilby91/medea/gen/medea/v1"
)

var (
	hostCluster string
	hostPool    string
	hostMAC     string
	hostRole    string
	hostLabels  []string
)

func init() {
	hostCmd := &cobra.Command{Use: "host", Short: "Manage the provisioning inventory (bare-metal hosts)"}

	register := &cobra.Command{
		Use:   "register",
		Short: "Register a bare-metal host (by MAC) into a cluster's inventory",
		Args:  cobra.NoArgs,
		RunE:  runHostRegister,
	}
	register.Flags().StringVar(&hostCluster, "cluster", "", "cluster name (required)")
	register.Flags().StringVar(&hostMAC, "mac", "", "host NIC MAC address (required; identity)")
	register.Flags().StringVar(&hostPool, "pool", "", "node pool the host joins when provisioned")
	register.Flags().StringVar(&hostRole, "role", "", "controlplane|worker (defaults to the pool's role)")
	register.Flags().StringArrayVar(&hostLabels, "label", nil, "label key=value (repeatable; matched by NodePool.selector)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List hosts in a cluster",
		Args:  cobra.NoArgs,
		RunE:  runHostList,
	}
	list.Flags().StringVar(&hostCluster, "cluster", "", "cluster name (required)")
	list.Flags().StringVar(&hostPool, "pool", "", "filter by node pool")

	deregister := &cobra.Command{
		Use:   "deregister",
		Short: "Remove a host from the inventory",
		Args:  cobra.NoArgs,
		RunE:  runHostDeregister,
	}
	deregister.Flags().StringVar(&hostCluster, "cluster", "", "cluster name (required)")
	deregister.Flags().StringVar(&hostMAC, "mac", "", "host NIC MAC address (required)")

	hostCmd.AddCommand(register, list, deregister)
	rootCmd.AddCommand(hostCmd)
}

func runHostRegister(_ *cobra.Command, _ []string) error {
	if hostCluster == "" || hostMAC == "" {
		return fmt.Errorf("--cluster and --mac are required")
	}
	role, err := parseRole(hostRole)
	if err != nil {
		return err
	}
	labels, err := parseLabels(hostLabels)
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

	h, err := c.RegisterHost(ctx, &pb.RegisterHostRequest{
		Cluster: hostCluster, Mac: hostMAC, Pool: hostPool, Role: role, Labels: labels,
	})
	if err != nil {
		return err
	}
	fmt.Printf("registered host %s (cluster %s, pool %s, role %s, state %s)\n",
		h.GetMac(), h.GetCluster(), dash(h.GetPool()), roleStr(h.GetRole()), hostStateStr(h.GetState()))
	return nil
}

func runHostList(_ *cobra.Command, _ []string) error {
	if hostCluster == "" {
		return errClusterRequired
	}
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	resp, err := c.ListHosts(ctx, &pb.ListHostsRequest{Cluster: hostCluster, Pool: hostPool})
	if err != nil {
		return err
	}
	tw := newTab()
	fmt.Fprintln(tw, "MAC\tPOOL\tROLE\tSTATE\tADDR\tLABELS")
	for _, h := range resp.GetHosts() {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			h.GetMac(), dash(h.GetPool()), roleStr(h.GetRole()),
			hostStateStr(h.GetState()), dash(h.GetAddr()), labelsStr(h.GetLabels()))
	}
	return tw.Flush()
}

func runHostDeregister(_ *cobra.Command, _ []string) error {
	if hostCluster == "" || hostMAC == "" {
		return fmt.Errorf("--cluster and --mac are required")
	}
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	if _, err := c.DeregisterHost(ctx, &pb.DeregisterHostRequest{Cluster: hostCluster, Mac: hostMAC}); err != nil {
		return err
	}
	fmt.Printf("deregistered host %s from cluster %s\n", hostMAC, hostCluster)
	return nil
}

func parseRole(s string) (pb.Role, error) {
	switch strings.ToLower(s) {
	case "":
		return pb.Role_ROLE_UNSPECIFIED, nil
	case "controlplane", "cp":
		return pb.Role_ROLE_CONTROLPLANE, nil
	case "worker":
		return pb.Role_ROLE_WORKER, nil
	default:
		return pb.Role_ROLE_UNSPECIFIED, fmt.Errorf("invalid --role %q (controlplane|worker)", s)
	}
}

func parseLabels(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --label %q (want key=value)", kv)
		}
		m[k] = v
	}
	return m, nil
}

func hostStateStr(s pb.HostState) string {
	return dash(strings.ToLower(strings.TrimPrefix(s.String(), "HOST_STATE_")))
}

func labelsStr(m map[string]string) string {
	if len(m) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
