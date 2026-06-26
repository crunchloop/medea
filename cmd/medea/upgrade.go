package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	pb "github.com/bilby91/medea/gen/medea/v1"
)

var (
	upCluster string
	upPool    string
	upTalos   string
	upConfirm bool
	upBy      string
)

func init() {
	upgradeCmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Plan (and with --confirm, start) a Talos rollout for a node pool",
		Long: "Without --confirm, upgrade prints a dry-run plan and changes nothing.\n" +
			"With --confirm it sets the pool's desired version and creates a Rollout job\n" +
			"(refused unless the cluster has rollouts enabled).",
		Args: cobra.NoArgs,
		RunE: runUpgrade,
	}
	f := upgradeCmd.Flags()
	f.StringVar(&upCluster, "cluster", "", "cluster name (required)")
	f.StringVar(&upPool, "pool", "", "node pool (required)")
	f.StringVar(&upTalos, "talos", "", "target Talos version, e.g. v1.13.6 (required)")
	f.BoolVar(&upConfirm, "confirm", false, "actually create the rollout (default: dry-run plan)")
	f.StringVar(&upBy, "by", "", "who is creating the rollout (audit; default $USER)")
	rootCmd.AddCommand(upgradeCmd)
}

func runUpgrade(_ *cobra.Command, _ []string) error {
	if upCluster == "" || upPool == "" || upTalos == "" {
		return fmt.Errorf("--cluster, --pool and --talos are required")
	}
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	// Plan: show each member's observed version vs the target.
	resp, err := c.ListMachines(ctx, &pb.ListMachinesRequest{Cluster: upCluster, Pool: upPool})
	if err != nil {
		return err
	}
	machines := resp.GetMachines()
	if len(machines) == 0 {
		return fmt.Errorf("no machines found in %s/%s", upCluster, upPool)
	}

	tw := newTab()
	fmt.Fprintf(tw, "PLAN  %s/%s  ->  talos %s\n", upCluster, upPool, upTalos)
	fmt.Fprintln(tw, "ENDPOINT\tROLE\tCURRENT\tTARGET\tACTION")
	toChange := 0
	for _, m := range machines {
		cur := m.GetObserved().GetTalosVersion()
		action := "upgrade"
		if cur == upTalos {
			action = "ok"
		} else {
			toChange++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.GetTalosEndpoint(), roleStr(m.GetRole()), dash(cur), upTalos, action)
	}
	tw.Flush()
	fmt.Printf("\n%d of %d node(s) would be upgraded.\n", toChange, len(machines))

	if !upConfirm {
		fmt.Println("Dry run — nothing created. Re-run with --confirm to start the rollout.")
		return nil
	}

	job, err := c.CreateRollout(ctx, &pb.CreateRolloutRequest{
		Cluster:       upCluster,
		Pool:          upPool,
		Kind:          pb.RolloutKind_ROLLOUT_KIND_TALOS,
		TargetVersion: upTalos,
		CreatedBy:     firstNonEmpty(upBy, os.Getenv("USER")),
	})
	if err != nil {
		return err
	}
	fmt.Printf("rollout created: %s/%s -> talos %s (state %s)\n",
		job.GetCluster(), job.GetPool(), job.GetTargetVersion(), rolloutJobStateStr(job.GetState()))
	fmt.Println("watch with: medea rollout status --cluster " + upCluster + " --pool " + upPool + " -w")
	return nil
}
