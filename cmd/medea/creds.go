package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/crunchloop/medea/internal/creds"
)

var (
	migrateCluster     string
	migrateFromDir     string
	migrateToVault     string
	migrateOpTokenFile string
)

func init() {
	credsCmd := &cobra.Command{Use: "creds", Short: "Manage cluster credentials"}

	migrate := &cobra.Command{
		Use:   "migrate",
		Short: "Copy a cluster's credentials from the file store into 1Password",
		Long: "migrate reads talosconfig/kubeconfig (and the captured secrets bundle, if\n" +
			"present) from the file-backed credential store and writes them into a\n" +
			"1Password vault — the cutover that lets home-cluster's _out/ be deleted\n" +
			"(design/credentials.md §4.1). Re-runnable; the secrets bundle is optional.",
		Args: cobra.NoArgs,
		RunE: runCredsMigrate,
	}
	f := migrate.Flags()
	f.StringVar(&migrateCluster, "cluster", "", "cluster name (required)")
	f.StringVar(&migrateFromDir, "from-dir", "medea-creds", "source file-backed credential store directory")
	f.StringVar(&migrateToVault, "op-vault", "Kubernetes", "destination 1Password vault")
	f.StringVar(&migrateOpTokenFile, "op-token-file", "", "file with a 1Password service-account token (required)")

	credsCmd.AddCommand(migrate)
	rootCmd.AddCommand(credsCmd)
}

func runCredsMigrate(_ *cobra.Command, _ []string) error {
	if migrateCluster == "" {
		return fmt.Errorf("--cluster is required")
	}
	tok, err := readTokenFile(migrateOpTokenFile, "--op-token-file")
	if err != nil {
		return err
	}

	from, err := creds.NewFileStore(migrateFromDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	vault, err := creds.NewOnePasswordSDKVault(ctx, tok)
	if err != nil {
		return err
	}
	to := creds.NewOnePasswordStore(migrateToVault, vault)

	if err := creds.Migrate(from, to, migrateCluster); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "migrated credentials for %q: %s -> 1Password vault %q (item medea-%s)\n",
		migrateCluster, migrateFromDir, migrateToVault, migrateCluster)
	return nil
}
