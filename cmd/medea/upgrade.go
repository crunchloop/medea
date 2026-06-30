package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	pb "github.com/crunchloop/medea/gen/medea/v1"
)

var (
	upCluster string
	upPool    string
	upTalos   string
	upK8s     string
	upConfirm bool
	upBy      string
)

func init() {
	upgradeCmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Plan (and with --confirm, start) a Talos or Kubernetes rollout",
		Long: "Without --confirm, upgrade prints a dry-run plan and changes nothing.\n" +
			"With --confirm it sets the desired version and creates a Rollout job\n" +
			"(refused unless the cluster has rollouts enabled).\n\n" +
			"--talos --pool <p> rolls a node pool's Talos OS version (node-by-node).\n" +
			"--k8s rolls the cluster's Kubernetes version (cluster-wide; no --pool).",
		Args: cobra.NoArgs,
		RunE: runUpgrade,
	}
	f := upgradeCmd.Flags()
	f.StringVar(&upCluster, "cluster", "", "cluster name (required)")
	f.StringVar(&upPool, "pool", "", "node pool (required for --talos)")
	f.StringVar(&upTalos, "talos", "", "target Talos version, e.g. v1.13.6")
	f.StringVar(&upK8s, "k8s", "", "target Kubernetes version, e.g. v1.36.2 (cluster-wide)")
	f.BoolVar(&upConfirm, "confirm", false, "actually create the rollout (default: dry-run plan)")
	f.StringVar(&upBy, "by", "", "who is creating the rollout (audit; default $USER)")
	rootCmd.AddCommand(upgradeCmd)
}

func runUpgrade(_ *cobra.Command, _ []string) error {
	if upCluster == "" {
		return fmt.Errorf("--cluster is required")
	}
	switch {
	case upK8s != "":
		if upTalos != "" || upPool != "" {
			return fmt.Errorf("--k8s is cluster-wide; do not combine it with --talos or --pool")
		}
		return runK8sUpgrade()
	case upTalos != "":
		if upPool == "" {
			return fmt.Errorf("--pool is required for a --talos rollout")
		}
		return runTalosUpgrade()
	default:
		return fmt.Errorf("specify --talos (with --pool) or --k8s")
	}
}

func runTalosUpgrade() error {
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

func runK8sUpgrade() error {
	c, closeFn, err := dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := cmdContext()
	defer cancel()

	// Plan: show the cluster's observed Kubernetes version vs the target.
	cl, err := c.GetCluster(ctx, &pb.GetClusterRequest{Cluster: upCluster})
	if err != nil {
		return err
	}
	cur := cl.GetObserved().GetKubernetesVersion()

	tw := newTab()
	fmt.Fprintf(tw, "PLAN  %s  ->  kubernetes %s  (cluster-wide; Talos-orchestrated)\n", upCluster, upK8s)
	fmt.Fprintln(tw, "CLUSTER\tCURRENT\tTARGET\tACTION")
	action := "upgrade"
	if cur == upK8s {
		action = "ok"
	}
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", upCluster, dash(cur), upK8s, action)
	tw.Flush()
	fmt.Println("\nA control-plane etcd snapshot is taken before the upgrade.")

	if !upConfirm {
		fmt.Println("Dry run — nothing created. Re-run with --confirm to start the rollout.")
		return nil
	}

	job, err := c.CreateRollout(ctx, &pb.CreateRolloutRequest{
		Cluster:       upCluster,
		Kind:          pb.RolloutKind_ROLLOUT_KIND_KUBERNETES,
		TargetVersion: upK8s,
		CreatedBy:     firstNonEmpty(upBy, os.Getenv("USER")),
	})
	if err != nil {
		return err
	}
	fmt.Printf("rollout created: %s -> kubernetes %s (state %s)\n",
		job.GetCluster(), job.GetTargetVersion(), rolloutJobStateStr(job.GetState()))
	fmt.Println("watch with: medea rollout status --cluster " + upCluster + " -w")
	return nil
}
