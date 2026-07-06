//go:build integration

// Package integration is the integration-test harness: it spins up a throwaway
// Talos cluster (docker provisioner) so the talos/kube wrappers and the rollout
// mechanics can be exercised against a real API (PRD §9.2, talos-client.md §9).
//
// Build-tagged `integration` so it never runs in the fast unit loop; run with
// `make test-integration` (needs docker + talosctl; minutes, not milliseconds).
//
// NOTE: shelling out to `talosctl` here is TEST INFRASTRUCTURE for standing up a
// scratch cluster — it is NOT the control plane's runtime behavior. Medea's
// production code never shells out (PRD §13 #15); it imports the Talos Go
// packages. The two are deliberately separate concerns.
package integration

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

// Options tunes the scratch cluster. The zero value matches Start (talosctl
// defaults). All fields are optional.
type Options struct {
	// K8sVersion pins the cluster's initial Kubernetes version (e.g. "v1.36.1"),
	// so a test can upgrade from a known lower patch. "" = talosctl default.
	K8sVersion string
	// TalosImage pins the Talos container image (e.g.
	// "ghcr.io/siderolabs/talos:v1.13.5"), to match the imported main-module
	// version. "" = talosctl default.
	TalosImage string
}

// Start brings up a single-control-plane, single-worker docker Talos cluster
// with talosctl defaults. See StartWith for tuning.
func Start(t *testing.T) *Cluster {
	t.Helper()
	return StartWith(t, Options{})
}

// StartWith brings up a single-control-plane, single-worker docker Talos
// cluster per opts, waits for it to be healthy, and registers teardown via
// t.Cleanup. If docker or talosctl is unavailable the test is skipped (so CI
// without them is green).
func StartWith(t *testing.T, opts Options) *Cluster {
	t.Helper()
	requireBin(t, "talosctl")
	requireBin(t, "docker")

	dir := t.TempDir()
	// Per-test cluster name: a create that gets KILLED (e.g. a slow runner hitting
	// the timeout below) leaks its docker containers; a shared name would then
	// wedge the NEXT test's create with "container name already in use". Distinct
	// names keep failures isolated to the test that caused them.
	name := clusterName(t)
	talosCfg := filepath.Join(dir, "talosconfig")
	kubeCfg := filepath.Join(dir, "kubeconfig")

	// Keep cluster state under the test's temp dir (not the default
	// ~/.talos/clusters) so runs are hermetic and don't depend on that dir's
	// ownership — a prior `sudo` run (e.g. the QEMU validation) can leave it
	// root-owned and unwritable.
	stateDir := filepath.Join(dir, "state")

	// Destroy first in case a previous run leaked, then create. talosctl v1.13
	// uses the `cluster create docker` subcommand (the old --provisioner flag is
	// gone); the docker provisioner is always 1 control plane and waits for the
	// cluster to be healthy before returning (no --wait flag).
	_ = runQuiet(10*time.Minute, "talosctl", "cluster", "destroy", "--name", name, "--state", stateDir)
	// Belt-and-suspenders: talosctl's destroy is scoped to the (fresh) --state
	// dir, so it can't see containers leaked by an earlier killed create. Sweep
	// them by docker name before creating.
	dockerCleanup(name)
	args := []string{"cluster", "create", "docker",
		"--name", name,
		"--workers", "1",
		"--talosconfig-destination", talosCfg,
		"--state", stateDir,
	}
	if opts.K8sVersion != "" {
		args = append(args, "--kubernetes-version", opts.K8sVersion)
	}
	if opts.TalosImage != "" {
		args = append(args, "--image", opts.TalosImage)
	}
	// 20m (was 12m): a cold or loaded host pulls the Talos image, boots two
	// nodes, bootstraps etcd, and waits for k8s health — slow runners were killed
	// right at the old ceiling while etcd was still converging.
	run(t, 20*time.Minute, "talosctl", args...)
	t.Cleanup(func() {
		_ = runQuiet(5*time.Minute, "talosctl", "cluster", "destroy", "--name", name, "--state", stateDir)
		dockerCleanup(name)
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

// clusterName derives a docker-safe, per-test cluster name from the test name
// (e.g. "medea-it-testdrainevictsworkload"), so leaked containers from one test
// can't collide with another's create.
func clusterName(t *testing.T) string {
	s := strings.ToLower(t.Name())
	s = strings.NewReplacer("/", "-", " ", "-", "_", "-").Replace(s)
	return "medea-it-" + s
}

// dockerCleanup force-removes any leftover docker containers and network for a
// talosctl docker cluster of the given name. `talosctl cluster destroy` is
// state-scoped (it reads the --state dir), so a create that was killed mid-flight
// leaves containers a fresh-state destroy can't see — which then wedges the next
// create with "container name already in use". Best-effort: errors are ignored.
func dockerCleanup(name string) {
	// talosctl names nodes "<cluster>-controlplane-1", "<cluster>-worker-1", …;
	// anchor on "^/<name>-" so we don't match an unrelated cluster by substring.
	if out, err := exec.Command("docker", "ps", "-aq", "--filter", "name=^/"+name+"-").Output(); err == nil {
		for _, id := range strings.Fields(string(out)) {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	}
	_ = exec.Command("docker", "network", "rm", name).Run()
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
