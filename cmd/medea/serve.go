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

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/bilby91/medea/gen/medea/v1"
	"github.com/bilby91/medea/internal/auth"
	"github.com/bilby91/medea/internal/server"
	"github.com/bilby91/medea/internal/store"
	"github.com/bilby91/medea/internal/tlsgen"
)

var (
	serveListen    string
	serveStore     string
	serveToken     string
	serveTokenFile string
	serveCert      string
	serveKey       string
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
	creds, err := credentials.NewServerTLSFromFile(serveCert, serveKey)
	if err != nil {
		return fmt.Errorf("load tls: %w", err)
	}

	st, err := store.Open(serveStore)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	srv := grpc.NewServer(
		grpc.Creds(creds),
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
