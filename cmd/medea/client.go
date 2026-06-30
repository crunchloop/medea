package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/crunchloop/medea/gen/medea/v1"
)

var (
	flagAddr     string
	flagToken    string
	flagCA       string
	flagInsecure bool
)

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&flagAddr, "addr", "", "server address (env MEDEA_ADDR; default localhost:7600)")
	pf.StringVar(&flagToken, "token", "", "bearer token (env MEDEA_TOKEN)")
	pf.StringVar(&flagCA, "ca", "", "server cert to trust as CA (env MEDEA_CA)")
	pf.BoolVar(&flagInsecure, "insecure", false, "disable TLS (dev only)")
}

// tokenCreds attaches the bearer token to each RPC. It refuses to send the
// token over an insecure transport unless explicitly running insecure.
type tokenCreds struct {
	token  string
	secure bool
}

func (c tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	if c.token == "" {
		return nil, nil
	}
	return map[string]string{"authorization": "Bearer " + c.token}, nil
}
func (c tokenCreds) RequireTransportSecurity() bool { return c.secure }

func dial() (pb.MedeaClient, func(), error) {
	addr := firstNonEmpty(flagAddr, os.Getenv("MEDEA_ADDR"), "localhost:7600")
	token := firstNonEmpty(flagToken, os.Getenv("MEDEA_TOKEN"))

	var tc credentials.TransportCredentials
	if flagInsecure {
		tc = insecure.NewCredentials()
	} else {
		ca := firstNonEmpty(flagCA, os.Getenv("MEDEA_CA"))
		if ca == "" {
			return nil, nil, errors.New("--ca <server cert> is required (or use --insecure for dev)")
		}
		pemBytes, err := os.ReadFile(ca)
		if err != nil {
			return nil, nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, nil, errors.New("no certificates found in --ca file")
		}
		tc = credentials.NewTLS(&tls.Config{RootCAs: pool})
	}

	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(tc),
		grpc.WithPerRPCCredentials(tokenCreds{token: token, secure: !flagInsecure}),
	)
	if err != nil {
		return nil, nil, err
	}
	return pb.NewMedeaClient(conn), func() { conn.Close() }, nil
}

func cmdContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
