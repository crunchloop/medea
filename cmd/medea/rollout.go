package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	pb "github.com/bilby91/medea/gen/medea/v1"
)

var errClusterRequired = errors.New("--cluster is required")

var (
	rolloutCluster string
	rolloutPool    string
	rolloutWatch   bool
)

func init() {
	rolloutCmd := &cobra.Command{Use: "rollout", Short: "Inspect rollouts"}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show rollout status for a cluster (optionally a pool)",
		Args:  cobra.NoArgs,
		RunE:  runRolloutStatus,
	}
	status.Flags().StringVar(&rolloutCluster, "cluster", "", "cluster name (required)")
	status.Flags().StringVar(&rolloutPool, "pool", "", "node pool")
	status.Flags().BoolVarP(&rolloutWatch, "watch", "w", false, "watch for changes")
	rolloutCmd.AddCommand(status)
	rootCmd.AddCommand(rolloutCmd)
}

func runRolloutStatus(_ *cobra.Command, _ []string) error {
	if rolloutCluster == "" {
		return errClusterRequired
	}
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()

	if err := printRollout(c); err != nil {
		return err
	}
	if !rolloutWatch {
		return nil
	}

	// Watch: on each change event, reprint the current rollout picture.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := c.Watch(ctx, &pb.WatchRequest{})
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			return err
		}
		// Only reprint on rollout-relevant changes for this cluster.
		if !strings.HasPrefix(ev.GetKey(), rolloutCluster) {
			continue
		}
		fmt.Printf("\n--- change (rev %d, %s) ---\n", ev.GetRevision(), ev.GetKind())
		if err := printRollout(c); err != nil {
			return err
		}
	}
}

func printRollout(c pb.MedeaClient) error {
	ctx, cancel := cmdContext()
	defer cancel()
	resp, err := c.GetRollout(ctx, &pb.GetRolloutRequest{Cluster: rolloutCluster, Pool: rolloutPool})
	if err != nil {
		return err
	}
	if cr := resp.GetClusterRollout(); cr != nil {
		fmt.Printf("kubernetes: %s -> %s  (%s)\n",
			dash(""), dash(cr.GetTargetKubernetesVersion()), clusterPhaseStr(cr.GetPhase()))
		if cr.GetMessage() != "" {
			fmt.Printf("  %s\n", cr.GetMessage())
		}
	}
	tw := newTab()
	fmt.Fprintln(tw, "MACHINE\tSTATE\tTARGET-TALOS\tMESSAGE")
	for _, mr := range resp.GetMachineRollouts() {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			mr.GetAddr(), rolloutStateStr(mr.GetState()),
			dash(mr.GetTargetTalosVersion()), dash(mr.GetMessage()))
	}
	return tw.Flush()
}

func clusterPhaseStr(p pb.ClusterRolloutPhase) string {
	return dash(strings.ToLower(strings.TrimPrefix(p.String(), "CLUSTER_ROLLOUT_PHASE_")))
}

func rolloutStateStr(s pb.RolloutState) string {
	return dash(strings.ToLower(strings.TrimPrefix(s.String(), "ROLLOUT_STATE_")))
}
