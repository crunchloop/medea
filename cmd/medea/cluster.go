package main

import (
	"fmt"

	"github.com/spf13/cobra"

	pb "github.com/bilby91/medea/gen/medea/v1"
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
	clusterCmd.AddCommand(enable, disable)
	rootCmd.AddCommand(clusterCmd)
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
