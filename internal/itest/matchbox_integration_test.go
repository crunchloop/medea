//go:build integration

package itest

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bilby91/medea/internal/provision"
	"github.com/bilby91/medea/internal/provision/matchbox"
)

// TestMatchboxServesStagedHost validates the driver↔Matchbox contract
// (design/provisioning-plane.md §3): our file-backed driver writes group /
// profile / generic files that a REAL Matchbox serves correctly — the
// generic_id field name and the talos.config arg in particular (the layout
// details unit tests can't prove). No VM/boot; just Matchbox in docker.
func TestMatchboxServesStagedHost(t *testing.T) {
	requireBin(t, "docker")

	const port = "38086"
	httpURL := "http://127.0.0.1:" + port
	dir := t.TempDir()

	st, err := matchbox.New(dir, httpURL)
	if err != nil {
		t.Fatal(err)
	}
	mac := "aa:bb:cc:dd:ee:ff"
	cfg := []byte("version: v1alpha1\nmachine:\n  type: worker\n  # MEDEA-TEST-MARKER\n")
	if err := st.Stage(context.Background(), mac, provision.Profile{
		Kernel: "https://factory.talos.dev/image/abc/v1.13.5/kernel-amd64",
		Initrd: []string{"https://factory.talos.dev/image/abc/v1.13.5/initramfs-amd64.xz"},
		Args:   []string{"talos.platform=metal"},
	}, cfg); err != nil {
		t.Fatalf("stage: %v", err)
	}

	name := "medea-matchbox-it"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	run(t, 3*time.Minute, "docker", "run", "-d", "--name", name, "--platform", "linux/amd64",
		"-p", port+":8080", "-v", dir+":/var/lib/matchbox:ro",
		"quay.io/poseidon/matchbox:v0.11.0", "-address=0.0.0.0:8080", "-log-level=debug")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	waitHTTP(t, httpURL+"/", 60*time.Second)

	q := "?mac=" + url.QueryEscape(mac)

	// /ipxe renders the boot script: our kernel + the talos.config pointing back
	// at /generic (this is what fetches the machine config on boot).
	ipxe := httpGet(t, httpURL+"/ipxe"+q)
	for _, want := range []string{"kernel-amd64", "talos.config=" + httpURL + "/generic"} {
		if !strings.Contains(ipxe, want) {
			t.Fatalf("/ipxe missing %q:\n%s", want, ipxe)
		}
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
