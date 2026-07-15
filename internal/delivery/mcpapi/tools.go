package mcpapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/crunchloop/medea/gen/medea/v1"
)

type emptyInput struct{}

type clusterInput struct {
	Cluster string `json:"cluster" jsonschema:"Medea cluster name"`
}

type poolInput struct {
	Cluster string `json:"cluster" jsonschema:"Medea cluster name"`
	Pool    string `json:"pool,omitempty" jsonschema:"Optional node-pool filter"`
}

func registerTools(server *mcp.Server, queries Queries) {
	mcp.AddTool(server, readTool("list_clusters", "List all clusters managed by Medea, including desired and observed versions, endpoints, revisions, and safety gates."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
			result, err := queries.ListClusters(ctx, &pb.ListClustersRequest{})
			return protoResult(result, fmt.Sprintf("Medea manages %d clusters.", len(result.GetClusters())), err)
		})

	mcp.AddTool(server, readTool("get_cluster", "Get one Medea cluster by name, including desired and observed versions, endpoints, revision, and rollout/provisioning safety gates."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input clusterInput) (*mcp.CallToolResult, any, error) {
			result, err := queries.GetCluster(ctx, &pb.GetClusterRequest{Cluster: input.Cluster})
			return protoResult(result, fmt.Sprintf("Retrieved cluster %q.", input.Cluster), err)
		})

	mcp.AddTool(server, readTool("list_node_pools", "List node pools for one Medea cluster, including members, desired Talos versions, rollout strategies, pause state, and provisioning selectors."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input clusterInput) (*mcp.CallToolResult, any, error) {
			result, err := queries.ListNodePools(ctx, &pb.ListNodePoolsRequest{Cluster: input.Cluster})
			return protoResult(result, fmt.Sprintf("Found %d node pools in cluster %q.", len(result.GetNodePools()), input.Cluster), err)
		})

	mcp.AddTool(server, readTool("list_machines", "List machines and current observed health/version state for one Medea cluster. Optionally restrict the result to one node pool."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input poolInput) (*mcp.CallToolResult, any, error) {
			result, err := queries.ListMachines(ctx, &pb.ListMachinesRequest{Cluster: input.Cluster, Pool: input.Pool})
			return protoResult(result, fmt.Sprintf("Found %d machines in cluster %q.", len(result.GetMachines()), input.Cluster), err)
		})

	mcp.AddTool(server, readTool("list_hosts", "List bare-metal provisioning hosts for one Medea cluster, including allocation, provisioning state, labels, and observed address. Optionally filter by pool."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input poolInput) (*mcp.CallToolResult, any, error) {
			result, err := queries.ListHosts(ctx, &pb.ListHostsRequest{Cluster: input.Cluster, Pool: input.Pool})
			return protoResult(result, fmt.Sprintf("Found %d provisioning hosts in cluster %q.", len(result.GetHosts()), input.Cluster), err)
		})

	mcp.AddTool(server, readTool("get_rollout", "Get current Kubernetes and per-machine rollout progress for one Medea cluster. Optionally restrict machine rollout progress to one node pool."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input poolInput) (*mcp.CallToolResult, any, error) {
			result, err := queries.GetRollout(ctx, &pb.GetRolloutRequest{Cluster: input.Cluster, Pool: input.Pool})
			return protoResult(result, fmt.Sprintf("Retrieved rollout state for cluster %q (%d machine records).", input.Cluster, len(result.GetMachineRollouts())), err)
		})

	mcp.AddTool(server, readTool("list_rollouts", "List persisted rollout jobs for one Medea cluster, including kind, target, planned machines, state, creator, and revision."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input clusterInput) (*mcp.CallToolResult, any, error) {
			result, err := queries.ListRollouts(ctx, &pb.ListRolloutsRequest{Cluster: input.Cluster})
			return protoResult(result, fmt.Sprintf("Found %d rollout jobs for cluster %q.", len(result.GetRollouts()), input.Cluster), err)
		})
}

func readTool(name, description string) *mcp.Tool {
	closedWorld := false
	return &mcp.Tool{
		Name: name, Description: description,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closedWorld,
		},
	}
}

func protoResult(message proto.Message, summary string, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return errorResult(err), nil, nil
	}
	encoded, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(message)
	if err != nil {
		return errorResult(err), nil, nil
	}
	var structured map[string]any
	if err := json.Unmarshal(encoded, &structured); err != nil {
		return errorResult(err), nil, nil
	}
	return &mcp.CallToolResult{
		StructuredContent: structured,
		Content:           []mcp.Content{&mcp.TextContent{Text: summary + " Use structuredContent for the complete result."}},
	}, nil, nil
}
