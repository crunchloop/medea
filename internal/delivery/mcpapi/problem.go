package mcpapi

import (
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Problem struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func errorResult(err error) *mcp.CallToolResult {
	problem := mapError(err)
	encoded, marshalErr := json.Marshal(problem)
	if marshalErr != nil {
		encoded = []byte(`{"code":"query_unavailable","detail":"Medea query is temporarily unavailable"}`)
	}
	return &mcp.CallToolResult{
		IsError: true, StructuredContent: problem,
		Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
	}
}

func mapError(err error) Problem {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return Problem{Code: "invalid_request", Detail: err.Error()}
	case codes.NotFound:
		return Problem{Code: "not_found", Detail: err.Error()}
	case codes.FailedPrecondition:
		return Problem{Code: "failed_precondition", Detail: err.Error()}
	case codes.Aborted:
		return Problem{Code: "conflict", Detail: err.Error()}
	case codes.Canceled, codes.DeadlineExceeded:
		return Problem{Code: "query_canceled", Detail: err.Error()}
	default:
		return Problem{Code: "query_unavailable", Detail: "Medea query is temporarily unavailable"}
	}
}
