package mcpapi_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/crunchloop/medea/gen/medea/v1"
	"github.com/crunchloop/medea/internal/delivery/mcpapi"
	"github.com/crunchloop/medea/internal/server"
	"github.com/crunchloop/medea/internal/store"
)

func TestReadOnlyCatalogAndStructuredResults(t *testing.T) {
	session, st := connect(t)
	seed(t, st)

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
			t.Fatalf("tool %q is not annotated read-only and idempotent: %+v", tool.Name, tool.Annotations)
		}
	}
	slices.Sort(names)
	want := []string{"get_cluster", "get_rollout", "list_clusters", "list_hosts", "list_machines", "list_node_pools", "list_rollouts"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	assertRequiredInputs(t, listed.Tools)

	tests := []struct {
		name      string
		arguments map[string]any
		validate  func(*testing.T, map[string]any)
	}{
		{"list_clusters", nil, func(t *testing.T, got map[string]any) {
			if len(objectSlice(t, got, "clusters")) != 1 {
				t.Fatalf("clusters = %#v", got)
			}
		}},
		{"get_cluster", map[string]any{"cluster": "home"}, func(t *testing.T, got map[string]any) {
			if got["name"] != "home" || got["mode"] != "CLUSTER_MODE_MANUAL" {
				t.Fatalf("cluster = %#v", got)
			}
		}},
		{"list_node_pools", map[string]any{"cluster": "home"}, func(t *testing.T, got map[string]any) {
			pools := objectSlice(t, got, "node_pools")
			if len(pools) != 2 {
				t.Fatalf("node_pools = %#v", pools)
			}
		}},
		{"list_machines", map[string]any{"cluster": "home", "pool": "workers"}, func(t *testing.T, got map[string]any) {
			machines := objectSlice(t, got, "machines")
			if len(machines) != 1 || machines[0]["pool"] != "workers" || machines[0]["observed"].(map[string]any)["phase"] != "MACHINE_PHASE_READY" {
				t.Fatalf("filtered machines = %#v", machines)
			}
		}},
		{"list_hosts", map[string]any{"cluster": "home", "pool": "workers"}, func(t *testing.T, got map[string]any) {
			hosts := objectSlice(t, got, "hosts")
			if len(hosts) != 1 || hosts[0]["mac"] != "aa:bb:cc:dd:ee:01" {
				t.Fatalf("filtered hosts = %#v", hosts)
			}
		}},
		{"get_rollout", map[string]any{"cluster": "home", "pool": "workers"}, func(t *testing.T, got map[string]any) {
			if got["cluster_rollout"].(map[string]any)["phase"] != "CLUSTER_ROLLOUT_PHASE_UPGRADING" {
				t.Fatalf("cluster rollout = %#v", got)
			}
			rollouts := objectSlice(t, got, "machine_rollouts")
			if len(rollouts) != 1 || rollouts[0]["state"] != "ROLLOUT_STATE_WAITING_HEALTHY" {
				t.Fatalf("machine rollouts = %#v", rollouts)
			}
		}},
		{"list_rollouts", map[string]any{"cluster": "home"}, func(t *testing.T, got map[string]any) {
			rollouts := objectSlice(t, got, "rollouts")
			if len(rollouts) != 1 || rollouts[0]["state"] != "ROLLOUT_JOB_STATE_RUNNING" {
				t.Fatalf("rollout jobs = %#v", rollouts)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := call(t, session, tc.name, tc.arguments)
			if result.IsError {
				t.Fatalf("tool error: %+v", result.StructuredContent)
			}
			structured, ok := result.StructuredContent.(map[string]any)
			if !ok {
				t.Fatalf("structuredContent type = %T", result.StructuredContent)
			}
			tc.validate(t, structured)
			text := result.Content[0].(*mcp.TextContent).Text
			if !strings.Contains(text, "Use structuredContent") || strings.Contains(text, "kubernetes_version") {
				t.Fatalf("text fallback is not compact: %q", text)
			}
		})
	}
}

func TestDomainErrorsAreToolResults(t *testing.T) {
	session, _ := connect(t)
	result := call(t, session, "get_cluster", map[string]any{"cluster": "missing"})
	if !result.IsError {
		t.Fatal("missing cluster did not return a tool execution error")
	}
	problem, ok := result.StructuredContent.(map[string]any)
	if !ok || problem["code"] != "not_found" {
		t.Fatalf("problem = %#v", result.StructuredContent)
	}
}

func TestRequiredArgumentsAreRejected(t *testing.T) {
	session, _ := connect(t)
	for _, name := range []string{"get_cluster", "list_node_pools", "list_machines", "list_hosts", "get_rollout", "list_rollouts"} {
		t.Run(name, func(t *testing.T) {
			result := call(t, session, name, map[string]any{})
			if !result.IsError {
				t.Fatal("missing cluster argument was accepted")
			}
		})
	}
}

func TestErrorMappingAndRedaction(t *testing.T) {
	queries := errorQueries{}
	session := connectQueries(t, queries)
	tests := []struct {
		cluster string
		code    string
	}{
		{"invalid", "invalid_request"},
		{"missing", "not_found"},
		{"precondition", "failed_precondition"},
		{"conflict", "conflict"},
		{"canceled", "query_canceled"},
		{"deadline", "query_canceled"},
		{"internal", "query_unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.cluster, func(t *testing.T) {
			result := call(t, session, "get_cluster", map[string]any{"cluster": tc.cluster})
			if !result.IsError {
				t.Fatal("error was returned as success")
			}
			problem := result.StructuredContent.(map[string]any)
			if problem["code"] != tc.code {
				t.Fatalf("problem = %#v", problem)
			}
			if tc.cluster == "internal" && strings.Contains(problem["detail"].(string), "database password") {
				t.Fatalf("internal detail leaked: %#v", problem)
			}
		})
	}
}

func TestConcurrentSessionsAndReconnect(t *testing.T) {
	st := mustStore(t)
	url := startServerURL(t, server.New(st))
	seed(t, st)

	first := connectClient(t, url)
	second := connectClient(t, url)
	if call(t, first, "list_clusters", nil).IsError || call(t, second, "list_clusters", nil).IsError {
		t.Fatal("concurrent session call failed")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if call(t, second, "get_cluster", map[string]any{"cluster": "home"}).IsError {
		t.Fatal("closing one session broke another")
	}
	reconnected := connectClient(t, url)
	if call(t, reconnected, "list_rollouts", map[string]any{"cluster": "home"}).IsError {
		t.Fatal("reconnected session call failed")
	}
}

func connect(t *testing.T) (*mcp.ClientSession, *store.BoltStore) {
	t.Helper()
	st := mustStore(t)
	return connectClient(t, startServerURL(t, server.New(st))), st
}

func connectQueries(t *testing.T, queries mcpapi.Queries) *mcp.ClientSession {
	t.Helper()
	return connectClient(t, startServerURL(t, queries))
}

func startServerURL(t *testing.T, queries mcpapi.Queries) string {
	t.Helper()
	srv, err := mcpapi.NewServer(queries)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

func connectClient(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "medea-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func seed(t *testing.T, st *store.BoltStore) {
	t.Helper()
	if _, err := st.PutClusterDesired(&pb.Cluster{
		Name: "home", Mode: pb.ClusterMode_CLUSTER_MODE_MANUAL,
		Desired: &pb.ClusterDesired{TalosVersion: "v1.13.5", KubernetesVersion: "v1.36.2"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	for _, pool := range []*pb.NodePool{
		{Cluster: "home", Name: "workers", Role: pb.Role_ROLE_WORKER, Members: []string{"10.0.0.11"}},
		{Cluster: "home", Name: "controlplane", Role: pb.Role_ROLE_CONTROLPLANE, Members: []string{"10.0.0.10"}},
	} {
		if _, err := st.PutNodePoolDesired(pool, 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, machine := range []*pb.Machine{
		{Cluster: "home", Pool: "workers", TalosEndpoint: "10.0.0.11", Role: pb.Role_ROLE_WORKER},
		{Cluster: "home", Pool: "controlplane", TalosEndpoint: "10.0.0.10", Role: pb.Role_ROLE_CONTROLPLANE},
	} {
		if _, err := st.PutMachineDesired(machine, 0); err != nil {
			t.Fatal(err)
		}
		st.SetMachineObserved(machine.GetCluster(), machine.GetTalosEndpoint(), &pb.MachineObserved{
			Phase: pb.MachinePhase_MACHINE_PHASE_READY, TalosVersion: "v1.13.5", Healthy: true,
		})
	}
	for _, host := range []*pb.Host{
		{Cluster: "home", Pool: "workers", Mac: "aa:bb:cc:dd:ee:01", State: pb.HostState_HOST_STATE_READY},
		{Cluster: "home", Pool: "controlplane", Mac: "aa:bb:cc:dd:ee:02", State: pb.HostState_HOST_STATE_READY},
	} {
		if _, err := st.PutHostDesired(host, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutClusterRollout(&pb.ClusterRollout{Cluster: "home", Phase: pb.ClusterRolloutPhase_CLUSTER_ROLLOUT_PHASE_UPGRADING}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutMachineRollout(&pb.MachineRollout{Cluster: "home", Addr: "10.0.0.11", State: pb.RolloutState_ROLLOUT_STATE_WAITING_HEALTHY}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRolloutJob(&pb.Rollout{Cluster: "home", Pool: "workers", Kind: pb.RolloutKind_ROLLOUT_KIND_TALOS, State: pb.RolloutJobState_ROLLOUT_JOB_STATE_RUNNING}); err != nil {
		t.Fatal(err)
	}
}

func mustStore(t *testing.T) *store.BoltStore {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "medea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func call(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s escaped as protocol error: %v", name, err)
	}
	return result
}

func objectSlice(t *testing.T, object map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := object[key].([]any)
	if !ok {
		t.Fatalf("%s type = %T in %#v", key, object[key], object)
	}
	out := make([]map[string]any, len(raw))
	for i := range raw {
		out[i] = raw[i].(map[string]any)
	}
	return out
}

func assertRequiredInputs(t *testing.T, tools []*mcp.Tool) {
	t.Helper()
	for _, tool := range tools {
		schema := tool.InputSchema.(map[string]any)
		required, _ := schema["required"].([]any)
		if tool.Name == "list_clusters" {
			if len(required) != 0 {
				t.Fatalf("list_clusters required = %v", required)
			}
			continue
		}
		if len(required) != 1 || required[0] != "cluster" {
			t.Fatalf("tool %q required = %v, want [cluster]", tool.Name, required)
		}
	}
}

type errorQueries struct{}

func (errorQueries) GetCluster(_ context.Context, req *pb.GetClusterRequest) (*pb.Cluster, error) {
	codesByCluster := map[string]codes.Code{
		"invalid": codes.InvalidArgument, "missing": codes.NotFound,
		"precondition": codes.FailedPrecondition, "conflict": codes.Aborted,
		"canceled": codes.Canceled, "deadline": codes.DeadlineExceeded,
		"internal": codes.Internal,
	}
	return nil, status.Error(codesByCluster[req.GetCluster()], "database password is hunter2")
}
func (errorQueries) ListClusters(context.Context, *pb.ListClustersRequest) (*pb.ListClustersResponse, error) {
	panic("unexpected call")
}
func (errorQueries) ListNodePools(context.Context, *pb.ListNodePoolsRequest) (*pb.ListNodePoolsResponse, error) {
	panic("unexpected call")
}
func (errorQueries) ListMachines(context.Context, *pb.ListMachinesRequest) (*pb.ListMachinesResponse, error) {
	panic("unexpected call")
}
func (errorQueries) ListHosts(context.Context, *pb.ListHostsRequest) (*pb.ListHostsResponse, error) {
	panic("unexpected call")
}
func (errorQueries) GetRollout(context.Context, *pb.GetRolloutRequest) (*pb.GetRolloutResponse, error) {
	panic("unexpected call")
}
func (errorQueries) ListRollouts(context.Context, *pb.ListRolloutsRequest) (*pb.ListRolloutsResponse, error) {
	panic("unexpected call")
}
