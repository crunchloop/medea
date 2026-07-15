package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServeMCPProcess exercises the composition root rather than only the MCP
// package: a real Medea child process generates TLS, opens bbolt, starts gRPC
// and /mcp, serves an SDK client, and shuts both listeners down on interrupt.
func TestServeMCPProcess(t *testing.T) {
	if os.Getenv("MEDEA_TEST_SERVE_MCP_HELPER") == "1" {
		runServeMCPHelper(t)
		return
	}

	grpcAddr := reserveAddress(t)
	mcpAddr := reserveAddress(t)
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeMCPProcess$")
	cmd.Env = append(os.Environ(),
		"MEDEA_TEST_SERVE_MCP_HELPER=1",
		"MEDEA_TEST_GRPC_ADDR="+grpcAddr,
		"MEDEA_TEST_MCP_ADDR="+mcpAddr,
		"MEDEA_TEST_DIR="+dir,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	endpoint := "http://" + mcpAddr + "/mcp"
	session := waitForMCP(t, endpoint, done, &output)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("list tools from child: %v\n%s", err, output.String())
	}
	if len(listed.Tools) != 7 {
		_ = cmd.Process.Kill()
		t.Fatalf("child advertised %d tools, want 7\n%s", len(listed.Tools), output.String())
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_clusters"})
	if err != nil || result.IsError {
		_ = cmd.Process.Kill()
		t.Fatalf("call list_clusters through child: result=%+v err=%v\n%s", result, err, output.String())
	}
	if err := session.Close(); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Medea child exited with error: %v\n%s", err, output.String())
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("Medea child did not shut down gracefully\n%s", output.String())
	}
}

func runServeMCPHelper(t *testing.T) {
	dir := os.Getenv("MEDEA_TEST_DIR")
	serveListen = os.Getenv("MEDEA_TEST_GRPC_ADDR")
	serveMCPListen = os.Getenv("MEDEA_TEST_MCP_ADDR")
	serveStore = filepath.Join(dir, "medea.db")
	serveToken = "test-token"
	serveTokenFile = ""
	serveCert = filepath.Join(dir, "cert.pem")
	serveKey = filepath.Join(dir, "key.pem")
	serveCredsDir = filepath.Join(dir, "creds")
	serveCredsBackend = "file"
	serveRefreshIntv = time.Hour
	serveRollouts = false
	serveProvisioning = false
	serveBootstrap = false
	if err := runServe(nil, nil); err != nil {
		t.Fatal(err)
	}
}

func reserveAddress(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitForMCP(t *testing.T, endpoint string, done <-chan error, output *bytes.Buffer) *mcp.ClientSession {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("Medea child exited before MCP became ready: %v\n%s", err, output.String())
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		client := mcp.NewClient(&mcp.Implementation{Name: "medea-process-test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
		cancel()
		if err == nil {
			return session
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("MCP did not become ready at %s\n%s", endpoint, output.String())
	return nil
}
