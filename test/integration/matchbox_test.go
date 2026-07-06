//go:build integration

package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/crunchloop/medea/internal/provision"
	"github.com/crunchloop/medea/internal/provision/matchbox"
)

// TestMatchboxServesStagedHost validates the driver↔Matchbox contract
// (design/provisioning-plane.md §3): our file-backed driver mirrors the boot
// assets and writes group / profile / generic files that a REAL Matchbox serves
// correctly — the generic_id field name, the talos.config arg, and the mirrored
// /assets URLs (layout details unit tests can't prove). No VM/boot; just Matchbox
// in docker.
//
// The boot assets come from a LOCAL httptest upstream, not the live Image
// Factory, so the test is hermetic and fast: Stage mirrors those bytes into the
// staging dir and Matchbox serves them back from /assets over plain HTTP.
func TestMatchboxServesStagedHost(t *testing.T) {
	requireBin(t, "docker")

	// Stand-in for the Talos Image Factory: any path returns a marker body, so
	// the mirror step has a real 200 upstream without touching the network.
	const kernelBody = "FAKE-KERNEL-BYTES"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, kernelBody+" "+r.URL.Path)
	}))
	t.Cleanup(upstream.Close)

	const port = "38086"
	httpURL := "http://127.0.0.1:" + port
	dir := t.TempDir()

	st, err := matchbox.New(dir, httpURL)
	if err != nil {
		t.Fatal(err)
	}
	mac := "aa:bb:cc:dd:ee:ff"
	cfg := []byte("version: v1alpha1\nmachine:\n  type: worker\n  # MEDEA-TEST-MARKER\n")
	const kernelPath = "/image/test/v1.13.5/kernel-amd64"
	if err := st.Stage(context.Background(), mac, provision.Profile{
		Kernel: upstream.URL + kernelPath,
		Initrd: []string{upstream.URL + "/image/test/v1.13.5/initramfs-amd64.xz"},
		Args:   []string{"talos.platform=metal"},
	}, cfg); err != nil {
		t.Fatalf("stage: %v", err)
	}

	name := "medea-matchbox-it"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	// Serve the mirrored boot assets from the default assets path
	// (/var/lib/matchbox/assets, created by Stage) — Medea's profiles now point
	// kernel/initrd at Matchbox's own /assets over plain HTTP (iPXE has no TLS),
	// so asset serving must be ENABLED (was disabled before the mirror feature).
	run(t, 3*time.Minute, "docker", "run", "-d", "--name", name, "--platform", "linux/amd64",
		"-p", port+":8080", "-v", dir+":/var/lib/matchbox:ro",
		"quay.io/poseidon/matchbox:v0.11.0", "-address=0.0.0.0:8080", "-log-level=debug")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	waitHTTP(t, httpURL+"/", 60*time.Second)

	q := "?mac=" + url.QueryEscape(mac)

	// /ipxe renders the boot script: the kernel points at Matchbox's mirrored
	// /assets URL (not the upstream), plus talos.config pointing back at /generic.
	ipxe := httpGet(t, httpURL+"/ipxe"+q)
	mirroredKernel := httpURL + "/assets" + kernelPath
	for _, want := range []string{mirroredKernel, "talos.config=" + httpURL + "/generic"} {
		if !strings.Contains(ipxe, want) {
			t.Fatalf("/ipxe missing %q:\n%s", want, ipxe)
		}
	}

	// Matchbox actually serves the mirrored kernel bytes at that /assets URL.
	if got := httpGet(t, mirroredKernel); !strings.Contains(got, kernelBody) {
		t.Fatalf("/assets did not serve the mirrored kernel: %q", got)
	}

	// /generic serves the exact machine config we staged for this MAC.
	gen := httpGet(t, httpURL+"/generic"+q)
	if !strings.Contains(gen, "MEDEA-TEST-MARKER") {
		t.Fatalf("/generic did not serve our config:\n%s", gen)
	}
}

func waitHTTP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(addr) //nolint:gosec,noctx // test against a local container
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("matchbox did not come up at %s within %s", addr, timeout)
}

func httpGet(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get(addr) //nolint:gosec,noctx // test against a local container
	if err != nil {
		t.Fatalf("GET %s: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("GET %s: status %d: %s", addr, resp.StatusCode, body)
	}
	return string(body)
}
