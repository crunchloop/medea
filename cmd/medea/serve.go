package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/crunchloop/medea/gen/medea/v1"
	"github.com/crunchloop/medea/internal/auth"
	"github.com/crunchloop/medea/internal/creds"
	"github.com/crunchloop/medea/internal/refresh"
	"github.com/crunchloop/medea/internal/rollout"
	"github.com/crunchloop/medea/internal/provision"
	"github.com/crunchloop/medea/internal/provision/matchbox"
	"github.com/crunchloop/medea/internal/server"
	"github.com/crunchloop/medea/internal/store"
	"github.com/crunchloop/medea/internal/talos/k8supgrade"
	"github.com/crunchloop/medea/internal/tlsgen"
)

// k8sUpgraderFactory builds the per-cluster Kubernetes upgrader from creds. It
// lives at the composition root so the heavy, version-coupled k8supgrade import
// (the Talos main module) stays out of internal/rollout (talos-client.md §4).
func k8sUpgraderFactory(cs creds.Store) rollout.K8sFactory {
	return func(_ context.Context, cl *pb.Cluster) (rollout.K8sOps, func(), error) {
		tb, err := cs.TalosConfig(cl.GetName())
		if err != nil {
			return nil, nil, err
		}
		up, err := k8supgrade.New(tb, cl.GetEndpoints().GetTalos())
		if err != nil {
			return nil, nil, err
		}
		return up, func() {}, nil
	}
}

var (
	serveListen      string
	serveStore       string
	serveToken       string
	serveTokenFile   string
	serveCert        string
	serveKey         string
	serveCredsDir    string
	serveRefreshIntv time.Duration
	serveRollouts    bool
	serveSnapshotDir string
	serveRolloutIntv time.Duration

	serveProvisioning  bool
	serveMatchboxDir   string
	serveMatchboxURL   string
	serveFactoryHost   string
	serveInstallDisk   string
	serveProvisionIntv time.Duration
	serveProvisionTO   time.Duration
)

func init() {
	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Medea control-plane server",
		Args:  cobra.NoArgs,
		RunE:  runServe,
	}
	f := serveCmd.Flags()
	f.StringVar(&serveListen, "listen", "0.0.0.0:7600", "listen address")
	f.StringVar(&serveStore, "store", "medea.db", "path to the bbolt state file")
	f.StringVar(&serveToken, "token", "", "bearer token (or use --token-file)")
	f.StringVar(&serveTokenFile, "token-file", "", "file containing the bearer token")
	f.StringVar(&serveCert, "tls-cert", "medea-cert.pem", "TLS cert path (generated if missing)")
	f.StringVar(&serveKey, "tls-key", "medea-key.pem", "TLS key path (generated if missing)")
	f.StringVar(&serveCredsDir, "creds-dir", "medea-creds", "directory holding cluster credentials")
	f.DurationVar(&serveRefreshIntv, "refresh-interval", 30*time.Second, "how often to refresh observed state from clusters")
	f.BoolVar(&serveRollouts, "rollouts", false, "enable the rollout executor (global gate; default off). Per-cluster rollouts-enabled still required.")
	f.StringVar(&serveSnapshotDir, "snapshot-dir", "medea-snapshots", "directory for pre-control-plane etcd snapshots")
	f.DurationVar(&serveRolloutIntv, "rollout-interval", 15*time.Second, "how often the rollout executor checks for jobs")
	f.BoolVar(&serveProvisioning, "provisioning", false, "enable the provisioning executor (global gate; default off). Per-cluster provisioning-enabled still required.")
	f.StringVar(&serveMatchboxDir, "matchbox-dir", "", "Matchbox data directory (required with --provisioning)")
	f.StringVar(&serveMatchboxURL, "matchbox-url", "", "Matchbox HTTP base URL nodes reach, e.g. http://host:8086 (required with --provisioning)")
	f.StringVar(&serveFactoryHost, "factory-host", provision.DefaultFactoryHost, "Talos Image Factory host")
	f.StringVar(&serveInstallDisk, "install-disk", "/dev/sda", "install disk for provisioned nodes")
	f.DurationVar(&serveProvisionIntv, "provision-interval", 30*time.Second, "how often the provisioning executor reconciles")
	f.DurationVar(&serveProvisionTO, "provision-timeout", 20*time.Minute, "how long a host may stay provisioning before it is marked failed")
	rootCmd.AddCommand(serveCmd)
}

func runServe(_ *cobra.Command, _ []string) error {
	token, err := resolveToken()
	if err != nil {
		return err
	}

	host, _, err := net.SplitHostPort(serveListen)
	if err != nil {
		return fmt.Errorf("invalid --listen %q: %w", serveListen, err)
	}
	var hosts []string
	if host != "" && host != "0.0.0.0" && host != "::" {
		hosts = append(hosts, host)
	}
	if err := tlsgen.EnsureServerCert(serveCert, serveKey, hosts); err != nil {
		return fmt.Errorf("tls cert: %w", err)
	}
	tlsCreds, err := credentials.NewServerTLSFromFile(serveCert, serveKey)
	if err != nil {
		return fmt.Errorf("load tls: %w", err)
	}

	st, err := store.Open(serveStore)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	srv := grpc.NewServer(
		grpc.Creds(tlsCreds),
		grpc.UnaryInterceptor(auth.UnaryInterceptor(token)),
		grpc.StreamInterceptor(auth.StreamInterceptor(token)),
	)
	pb.RegisterMedeaServer(srv, server.New(st))

	lis, err := net.Listen("tcp", serveListen)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Refresh loop: rebuild observed state from the clusters (datastore.md §7).
	cs, err := creds.NewFileStore(serveCredsDir)
	if err != nil {
		return fmt.Errorf("creds: %w", err)
	}
	go refresh.New(st, refresh.CredsFactory(cs), serveRefreshIntv).Run(ctx)

	// Rollout executor: global gate, default off. Even when on, per-cluster
	// rollouts-enabled is still required (rollout-safety.md §3).
	if serveRollouts {
		go rollout.NewExecutor(st, rollout.CredsFactory(cs), serveSnapshotDir, serveRolloutIntv).
			WithK8sFactory(k8sUpgraderFactory(cs)).
			Run(ctx)
		fmt.Fprintln(os.Stderr, "medea: rollout executor ENABLED (per-cluster rollouts-enabled still required)")
	} else {
		fmt.Fprintln(os.Stderr, "medea: rollout executor disabled (pass --rollouts to enable)")
	}

	// Provisioning executor: global gate, default off. Per-cluster
	// provisioning-enabled is still required (provisioning-plane.md §4).
	if serveProvisioning {
		if serveMatchboxDir == "" || serveMatchboxURL == "" {
			return fmt.Errorf("--provisioning requires --matchbox-dir and --matchbox-url")
		}
		mb, err := matchbox.New(serveMatchboxDir, serveMatchboxURL)
		if err != nil {
			return fmt.Errorf("matchbox: %w", err)
		}
		go provision.NewExecutor(st, mb, provision.NewFactoryClient(serveFactoryHost),
			provision.CredsKubeFactory(cs), provision.CredsSecretsFunc(cs),
			serveFactoryHost, serveInstallDisk, serveProvisionTO, serveProvisionIntv).Run(ctx)
		fmt.Fprintln(os.Stderr, "medea: provisioning executor ENABLED (per-cluster provisioning-enabled still required)")
	} else {
		fmt.Fprintln(os.Stderr, "medea: provisioning executor disabled (pass --provisioning to enable)")
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()
	fmt.Fprintf(os.Stderr, "medea: serving on %s (store=%s, tls cert=%s)\n", serveListen, serveStore, serveCert)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "medea: shutting down")
		srv.GracefulStop()
		return nil
	}
}

func resolveToken() (string, error) {
	if serveToken != "" {
		return serveToken, nil
	}
	if serveTokenFile != "" {
		b, err := os.ReadFile(serveTokenFile)
		if err != nil {
			return "", err
		}
		t := strings.TrimSpace(string(b))
		if t == "" {
			return "", errors.New("token file is empty")
		}
		return t, nil
	}
	return "", errors.New("--token or --token-file is required")
}
