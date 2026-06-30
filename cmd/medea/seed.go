package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/crunchloop/medea/internal/creds"
	"github.com/crunchloop/medea/internal/kube"
	"github.com/crunchloop/medea/internal/seed"
	"github.com/crunchloop/medea/internal/store"
	"github.com/crunchloop/medea/internal/talos"
)

var (
	seedClusterName string
	seedTalosconfig string
	seedKubeconfig  string
	seedStore       string
	seedCredsDir    string
	seedEndpoints   []string
)

func init() {
	seedCmd := &cobra.Command{
		Use:   "seed",
		Short: "Bootstrap the store from a live cluster (run while the server is stopped)",
		Long: "seed reads a cluster's current state via its talosconfig/kubeconfig, stores the\n" +
			"credentials, and writes the Cluster/NodePool/Machine records (desired = current\n" +
			"reality, so no rollout is triggered). Opens the bbolt store directly, so the\n" +
			"server must not be running.",
		Args: cobra.NoArgs,
		RunE: runSeed,
	}
	f := seedCmd.Flags()
	f.StringVar(&seedClusterName, "cluster", "", "cluster name to register (required)")
	f.StringVar(&seedTalosconfig, "talosconfig", "", "path to talosconfig (required)")
	f.StringVar(&seedKubeconfig, "kubeconfig", "", "path to kubeconfig (required)")
	f.StringVar(&seedStore, "store", "medea.db", "path to the bbolt state file")
	f.StringVar(&seedCredsDir, "creds-dir", "medea-creds", "directory to store cluster credentials")
	f.StringSliceVar(&seedEndpoints, "endpoint", nil, "Talos endpoints (default: control-plane node IPs)")
	rootCmd.AddCommand(seedCmd)
}

func runSeed(_ *cobra.Command, _ []string) error {
	if seedClusterName == "" || seedTalosconfig == "" || seedKubeconfig == "" {
		return fmt.Errorf("--cluster, --talosconfig and --kubeconfig are required")
	}
	talosBytes, err := os.ReadFile(seedTalosconfig)
	if err != nil {
		return err
	}
	kubeBytes, err := os.ReadFile(seedKubeconfig)
	if err != nil {
		return err
	}

	// Persist credentials (out of the bbolt store, api-and-auth.md §5).
	cs, err := creds.NewFileStore(seedCredsDir)
	if err != nil {
		return err
	}
	if err := cs.Put(seedClusterName, talosBytes, kubeBytes); err != nil {
		return fmt.Errorf("store credentials: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	kc, err := kube.New(kubeBytes)
	if err != nil {
		return err
	}
	nodes, err := kc.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	kubeHost, err := kube.ServerHost(kubeBytes)
	if err != nil {
		return err
	}

	endpoints := seedEndpoints
	if len(endpoints) == 0 {
		for _, n := range nodes {
			if n.Role == "controlplane" {
				endpoints = append(endpoints, n.InternalIP)
			}
		}
	}

	tc, err := talos.New(ctx, talosBytes, endpoints)
	if err != nil {
		return err
	}
	defer tc.Close()

	st, err := store.Open(seedStore)
	if err != nil {
		return fmt.Errorf("open store (is the server running?): %w", err)
	}
	defer st.Close()

	res, err := seed.Apply(st, seed.Inputs{
		Cluster:        seedClusterName,
		KubeEndpoint:   kubeHost,
		TalosEndpoints: endpoints,
		Nodes:          nodes,
		TalosVersion:   func(addr string) (string, error) { return tc.Version(ctx, addr) },
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "seeded cluster %q: %d machines in pools %v (talos %s, k8s %s)\n",
		res.Cluster, res.Machines, res.Pools, res.TalosSeed, res.K8sSeed)
	return nil
}
