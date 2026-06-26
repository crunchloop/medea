package server_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/bilby91/medea/gen/medea/v1"
	"github.com/bilby91/medea/internal/auth"
	"github.com/bilby91/medea/internal/server"
	"github.com/bilby91/medea/internal/store"
)

const serverToken = "s3cret"

type tokenCreds struct{ token string }

func (c tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	if c.token == "" {
		return nil, nil
	}
	return map[string]string{"authorization": "Bearer " + c.token}, nil
}
func (tokenCreds) RequireTransportSecurity() bool { return false }

// newClient wires a real BoltStore + auth interceptors + server over an
// in-memory bufconn, returning a client that presents clientToken.
func newClient(t *testing.T, clientToken string) (pb.MedeaClient, *store.BoltStore) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(auth.UnaryInterceptor(serverToken)),
		grpc.StreamInterceptor(auth.StreamInterceptor(serverToken)),
	)
	pb.RegisterMedeaServer(srv, server.New(st))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(tokenCreds{token: clientToken}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewMedeaClient(conn), st
}

func seedCluster(t *testing.T, st *store.BoltStore, name, k8s string) {
	t.Helper()
	if _, err := st.PutClusterDesired(&pb.Cluster{
		Name:    name,
		Desired: &pb.ClusterDesired{TalosVersion: "v1.13.5", KubernetesVersion: k8s},
	}, 0); err != nil {
		t.Fatal(err)
	}
}

func TestAuth(t *testing.T) {
	ctx := context.Background()

	// wrong token
	bad, _ := newClient(t, "wrong")
	if _, err := bad.ListClusters(ctx, &pb.ListClustersRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong token: code=%v, want Unauthenticated", status.Code(err))
	}

	// no token
	none, _ := newClient(t, "")
	if _, err := none.ListClusters(ctx, &pb.ListClustersRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no token: code=%v, want Unauthenticated", status.Code(err))
	}

	// correct token
	good, _ := newClient(t, serverToken)
	if _, err := good.ListClusters(ctx, &pb.ListClustersRequest{}); err != nil {
		t.Fatalf("correct token rejected: %v", err)
	}
}

func TestReads(t *testing.T) {
	ctx := context.Background()
	c, st := newClient(t, serverToken)
	seedCluster(t, st, "home", "v1.36.1")

	list, err := c.ListClusters(ctx, &pb.ListClustersRequest{})
	if err != nil || len(list.GetClusters()) != 1 {
		t.Fatalf("list: %v / %d", err, len(list.GetClusters()))
	}
	got, err := c.GetCluster(ctx, &pb.GetClusterRequest{Cluster: "home"})
	if err != nil || got.GetDesired().GetKubernetesVersion() != "v1.36.1" {
		t.Fatalf("get: %v / %q", err, got.GetDesired().GetKubernetesVersion())
	}
	if _, err := c.GetCluster(ctx, &pb.GetClusterRequest{Cluster: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing cluster: code=%v, want NotFound", status.Code(err))
	}
}

func TestSetClusterVersionsPartialUpdate(t *testing.T) {
	ctx := context.Background()
	c, st := newClient(t, serverToken)
	seedCluster(t, st, "home", "v1.36.1") // talos v1.13.5

	// Set only kubernetes_version; talos must be preserved.
	k8s := "v1.36.2"
	resp, err := c.SetClusterVersions(ctx, &pb.SetClusterVersionsRequest{
		Cluster:           "home",
		KubernetesVersion: &k8s,
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := c.GetCluster(ctx, &pb.GetClusterRequest{Cluster: "home"})
	if got.GetDesired().GetKubernetesVersion() != "v1.36.2" {
		t.Fatalf("k8s not updated: %q", got.GetDesired().GetKubernetesVersion())
	}
	if got.GetDesired().GetTalosVersion() != "v1.13.5" {
		t.Fatalf("talos clobbered: %q", got.GetDesired().GetTalosVersion())
	}
	if resp.GetRevision() != got.GetRevision() {
		t.Fatalf("returned revision %d != record %d", resp.GetRevision(), got.GetRevision())
	}
}

func TestSetClusterVersionsCASMismatch(t *testing.T) {
	ctx := context.Background()
	c, st := newClient(t, serverToken)
	seedCluster(t, st, "home", "v1.36.1")

	k8s := "v1.36.2"
	_, err := c.SetClusterVersions(ctx, &pb.SetClusterVersionsRequest{
		Cluster:           "home",
		KubernetesVersion: &k8s,
		ExpectedRevision:  999, // stale
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale CAS: code=%v, want FailedPrecondition", status.Code(err))
	}
}

func TestSetClusterVersionsNotFound(t *testing.T) {
	ctx := context.Background()
	c, _ := newClient(t, serverToken)
	k8s := "v1.36.2"
	_, err := c.SetClusterVersions(ctx, &pb.SetClusterVersionsRequest{Cluster: "ghost", KubernetesVersion: &k8s})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code=%v, want NotFound", status.Code(err))
	}
}

func TestWatchSnapshotThenLive(t *testing.T) {
	c, st := newClient(t, serverToken)
	seedCluster(t, st, "home", "v1.36.1") // revision 1

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := c.Watch(ctx, &pb.WatchRequest{SinceRevision: 0})
	if err != nil {
		t.Fatal(err)
	}

	// snapshot event for the seeded cluster
	ev, err := stream.Recv()
	if err != nil || ev.GetRevision() != 1 || ev.GetKind() != "cluster" {
		t.Fatalf("snapshot event: %v / %+v", err, ev)
	}

	// live event after a mutation
	k8s := "v1.36.2"
	if _, err := c.SetClusterVersions(ctx, &pb.SetClusterVersionsRequest{Cluster: "home", KubernetesVersion: &k8s}); err != nil {
		t.Fatal(err)
	}
	ev, err = stream.Recv()
	if err != nil || ev.GetRevision() != 2 {
		t.Fatalf("live event: %v / %+v", err, ev)
	}
}
