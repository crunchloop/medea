//go:build integration

// Package itest is the integration-test harness: it spins up a throwaway Talos
// cluster (docker provisioner) so the talos/kube wrappers and the rollout
// mechanics can be exercised against a real API (PRD §9.2, talos-client.md §9).
//
// Build-tagged `integration` so it never runs in the fast unit loop; run with
// `make test-integration` (needs docker + talosctl; minutes, not milliseconds).
//
// NOTE: shelling out to `talosctl` here is TEST INFRASTRUCTURE for standing up a
// scratch cluster — it is NOT the control plane's runtime behavior. Medea's
// production code never shells out (PRD §13 #15); it imports the Talos Go
// packages. The two are deliberately separate concerns.
package itest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Cluster is a running scratch Talos cluster and its credentials.
type Cluster struct {
	Name            string
	TalosconfigPath string
	KubeconfigPath  string
	Talosconfig     []byte
	Kubeconfig      []byte
}

// Start brings up a single-control-plane, single-worker docker Talos cluster,
// waits for it to be healthy, and registers teardown via t.Cleanup. If docker
// or talosctl is unavailable the test is skipped (so CI without them is green).
func Start(t *testing.T) *Cluster {
	t.Helper()
	requireBin(t, "talosctl")
	requireBin(t, "docker")

	dir := t.TempDir()
	name := "medea-it"
	talosCfg := filepath.Join(dir, "talosconfig")
	kubeCfg := filepath.Join(dir, "kubeconfig")

	// Destroy first in case a previous run leaked, then create. talosctl v1.13
	// uses the `cluster create docker` subcommand (the old --provisioner flag is
	// gone); the docker provisioner is always 1 control plane and waits for the
	// cluster to be healthy before returning (no --wait flag).
	_ = runQuiet(10*time.Minute, "talosctl", "cluster", "destroy", "--name", name)
	run(t, 12*time.Minute, "talosctl", "cluster", "create", "docker",
		"--name", name,
		"--workers", "1",
		"--talosconfig-destination", talosCfg,
	)
	t.Cleanup(func() {
		_ = runQuiet(5*time.Minute, "talosctl", "cluster", "destroy", "--name", name)
	})

	tb, err := os.ReadFile(talosCfg)
	if err != nil {
		t.Fatalf("read talosconfig: %v", err)
	}
	// `talosctl kubeconfig` needs an explicit node — and it wants the node's
	// cluster IP (e.g. 10.5.0.2), not the talosconfig endpoint, which on macOS
	// Docker Desktop is a host-mapped 127.0.0.1:<port>. Get the control-plane
	// container's network IP from docker.
	cpNode := controlPlaneNodeIP(t, name)
	run(t, 2*time.Minute, "talosctl", "--talosconfig", talosCfg, "kubeconfig", "--force", "--nodes", cpNode, kubeCfg)

	kb, err := os.ReadFile(kubeCfg)
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}
	return &Cluster{
		Name:            name,
		TalosconfigPath: talosCfg,
		KubeconfigPath:  kubeCfg,
		Talosconfig:     tb,
		Kubeconfig:      kb,
	}
}

// controlPlaneNodeIP returns the control-plane container's IP on the Talos
// docker network (the node identity talosctl/Talos use for --nodes routing).
func controlPlaneNodeIP(t *testing.T, clusterName string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
		clusterName+"-controlplane-1").Output()
	if err != nil {
		t.Fatalf("docker inspect control-plane container: %v", err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		t.Fatal("control-plane container has no network IP")
	}
	return ip
}

func requireBin(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("integration: %q not found on PATH; skipping", name)
	}
}

func run(t *testing.T, timeout time.Duration, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	t.Logf("$ %s %v", name, args)
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out.String())
	}
}

func runQuiet(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run()
}
