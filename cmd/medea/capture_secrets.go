package main

import (
	"context"
	"fmt"
	"os"
	"time"

	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/spf13/cobra"

	"github.com/crunchloop/medea/internal/creds"
	"github.com/crunchloop/medea/internal/talos"
)

var (
	capCluster  string
	capCredsDir string
	capNode     string
)

func init() {
	cmd := &cobra.Command{
		Use:   "capture-secrets",
		Short: "Capture a cluster's machine-secrets bundle into the credential store (for provisioning)",
		Long: "capture-secrets reads a control-plane node's machine config via the Talos API\n" +
			"and extracts the EXISTING cluster's secrets bundle (CA, tokens) as secrets.yaml in\n" +
			"the credential store, so v2 provisioning can mint join configs for new nodes. It\n" +
			"does not generate new secrets. Read-only against the cluster; re-runnable. Reads\n" +
			"the talosconfig already stored by `medea seed`.",
		Args: cobra.NoArgs,
		RunE: runCaptureSecrets,
	}
	f := cmd.Flags()
	f.StringVar(&capCluster, "cluster", "", "cluster name (required)")
	f.StringVar(&capCredsDir, "creds-dir", "medea-creds", "credential store directory (holds talosconfig; receives secrets.yaml)")
	f.StringVar(&capNode, "node", "", "control-plane node to read from (default: first talosconfig endpoint)")
	rootCmd.AddCommand(cmd)
}

func runCaptureSecrets(_ *cobra.Command, _ []string) error {
	if capCluster == "" {
		return fmt.Errorf("--cluster is required")
	}
	cs, err := creds.NewFileStore(capCredsDir)
	if err != nil {
		return err
	}
	talosBytes, err := cs.TalosConfig(capCluster)
	if err != nil {
		return fmt.Errorf("read talosconfig (run `medea seed` first?): %w", err)
	}

	node := capNode
	if node == "" {
		cfg, err := talosconfig.FromBytes(talosBytes)
		if err != nil {
			return fmt.Errorf("parse talosconfig: %w", err)
		}
		if cur := cfg.Contexts[cfg.Context]; cur != nil && len(cur.Endpoints) > 0 {
			node = cur.Endpoints[0]
		}
	}
	if node == "" {
		return fmt.Errorf("no --node given and no endpoint in talosconfig")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tc, err := talos.New(ctx, talosBytes, []string{node})
	if err != nil {
		return err
	}
	defer tc.Close()

	bundle, err := tc.CaptureSecrets(ctx, node)
	if err != nil {
		return err
	}
	if err := cs.PutSecrets(capCluster, bundle); err != nil {
		return fmt.Errorf("store secrets: %w", err)
	}
	fmt.Fprintf(os.Stderr, "captured secrets bundle for cluster %q from %s -> %s/%s/secrets.yaml\n",
		capCluster, node, capCredsDir, capCluster)
	return nil
}
