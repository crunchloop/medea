// Package mcpapi exposes Medea's read-only Model Context Protocol delivery
// adapter. MCP SDK types are deliberately confined to this package.
package mcpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pb "github.com/crunchloop/medea/gen/medea/v1"
)

const ServerVersion = "1.0.0"

// Queries is the existing Medea application read surface consumed by MCP.
// internal/server.Server satisfies this interface; MCP does not read the store
// directly or introduce a second application API.
type Queries interface {
	GetCluster(context.Context, *pb.GetClusterRequest) (*pb.Cluster, error)
	ListClusters(context.Context, *pb.ListClustersRequest) (*pb.ListClustersResponse, error)
	ListNodePools(context.Context, *pb.ListNodePoolsRequest) (*pb.ListNodePoolsResponse, error)
	ListMachines(context.Context, *pb.ListMachinesRequest) (*pb.ListMachinesResponse, error)
	ListHosts(context.Context, *pb.ListHostsRequest) (*pb.ListHostsResponse, error)
	GetRollout(context.Context, *pb.GetRolloutRequest) (*pb.GetRolloutResponse, error)
	ListRollouts(context.Context, *pb.ListRolloutsRequest) (*pb.ListRolloutsResponse, error)
}

// Server owns the MCP protocol server while keeping transport types out of the
// composition root.
type Server struct {
	server *mcp.Server
}

func NewServer(queries Queries) (*Server, error) {
	if queries == nil {
		return nil, errors.New("MCP server requires Medea queries")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "medea", Version: ServerVersion}, nil)
	registerTools(server, queries)
	return &Server{server: server}, nil
}

// Handler serves MCP over Streamable HTTP. The handler is stateful so clients
// may negotiate ordinary MCP sessions; tool execution itself remains stateless.
func (s *Server) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return s.server
	}, nil)
}
