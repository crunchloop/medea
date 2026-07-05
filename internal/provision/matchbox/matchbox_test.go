package matchbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crunchloop/medea/internal/provision"
)

func TestStageWritesGroupProfileAndConfig(t *testing.T) {
	// A stand-in Image Factory serving the kernel/initramfs Medea mirrors.
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("asset-bytes:" + r.URL.Path))
	}))
	defer factory.Close()
	kURL := factory.URL + "/image/xyz/v1.13.5/kernel-amd64"
	iURL := factory.URL + "/image/xyz/v1.13.5/initramfs-amd64.xz"

	root := t.TempDir()
	s, err := New(root, "http://matchbox:8086/")
	if err != nil {
		t.Fatal(err)
	}
	cfg := []byte("machine:\n  type: worker\n")
	err = s.Stage(context.Background(), "AA:BB:CC:DD:EE:FF", provision.Profile{
		Kernel: kURL,
		Initrd: []string{iURL},
		Args:   []string{"talos.platform=metal", "console=ttyS0"},
	}, cfg)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	key := "aa-bb-cc-dd-ee-ff"

	// generic machine config, 0600.
	gpath := filepath.Join(root, genericDir, key)
	got, err := os.ReadFile(gpath)
	if err != nil || string(got) != string(cfg) {
		t.Fatalf("generic config: %v / %q", err, got)
	}
	if fi, _ := os.Stat(gpath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("generic config perm = %o, want 600", fi.Mode().Perm())
	}

	// group selects the MAC and points at the profile.
	var g group
	readJSON(t, filepath.Join(root, groupsDir, key+".json"), &g)
	if g.Profile != key || g.Selector["mac"] != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("group wrong: %+v", g)
	}

	// profile references the generic config and points boot at MIRRORED assets —
	// Matchbox HTTP URLs, not the factory HTTPS ones (iPXE has no TLS).
	var p profile
	readJSON(t, filepath.Join(root, profilesDir, key+".json"), &p)
	wantKernel := "http://matchbox:8086/assets/image/xyz/v1.13.5/kernel-amd64"
	wantInitrd := "http://matchbox:8086/assets/image/xyz/v1.13.5/initramfs-amd64.xz"
	if p.Boot.Kernel != wantKernel || len(p.Boot.Initrd) != 1 || p.Boot.Initrd[0] != wantInitrd || p.GenericID != key {
		t.Fatalf("profile wrong: %+v", p)
	}

	// the assets were actually mirrored to disk (under /assets, mirroring the source path).
	kBytes, err := os.ReadFile(filepath.Join(root, assetsDir, "image/xyz/v1.13.5/kernel-amd64"))
	if err != nil || string(kBytes) != "asset-bytes:/image/xyz/v1.13.5/kernel-amd64" {
		t.Fatalf("kernel not mirrored: %v / %q", err, kBytes)
	}
	// caller args are preserved and a talos.config arg pointing at Matchbox is added.
	joined := strings.Join(p.Boot.Args, " ")
	for _, want := range []string{"talos.platform=metal", "console=ttyS0", "talos.config=http://matchbox:8086/generic?mac=${net0/mac:hexhyp}"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("profile args missing %q: %v", want, p.Boot.Args)
		}
	}

	// the JSON field must be "generic_id" (what Matchbox actually reads).
	raw, _ := os.ReadFile(filepath.Join(root, profilesDir, key+".json"))
	if !strings.Contains(string(raw), `"generic_id"`) {
		t.Fatalf("profile JSON missing generic_id field:\n%s", raw)
	}
}

func TestMirrorCachesAndPassesThroughLocal(t *testing.T) {
	var hits int
	factory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("k"))
	}))
	defer factory.Close()

	root := t.TempDir()
	s, _ := New(root, "http://matchbox:8086")
	kURL := factory.URL + "/image/a/v/kernel-amd64"

	// Two stages of the same asset → downloaded once (immutable, cache hit).
	for i := 0; i < 2; i++ {
		if err := s.Stage(context.Background(), "aa:bb:cc:dd:ee:0"+string(rune('0'+i)), provision.Profile{Kernel: kURL}, []byte("x")); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
	}
	if hits != 1 {
		t.Fatalf("expected 1 download (cached), got %d", hits)
	}

	// An asset already served by this Matchbox is passed through, not re-mirrored.
	already := "http://matchbox:8086/assets/image/a/v/kernel-amd64"
	got, err := s.mirror(context.Background(), already)
	if err != nil || got != already {
		t.Fatalf("expected passthrough of own URL, got %q (%v)", got, err)
	}
	// A non-HTTP reference (relative/local) is passed through untouched.
	if got, _ := s.mirror(context.Background(), "vmlinuz"); got != "vmlinuz" {
		t.Fatalf("expected passthrough of local ref, got %q", got)
	}
}

func TestUnstageRemovesAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	s, _ := New(root, "http://matchbox:8086")
	mac := "aa:bb:cc:dd:ee:ff"
	if err := s.Stage(context.Background(), mac, provision.Profile{Kernel: "k"}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Unstage(context.Background(), mac); err != nil {
		t.Fatalf("unstage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, groupsDir, "aa-bb-cc-dd-ee-ff.json")); !os.IsNotExist(err) {
		t.Fatalf("group still present: %v", err)
	}
	// second unstage is a no-op, not an error.
	if err := s.Unstage(context.Background(), mac); err != nil {
		t.Fatalf("second unstage: %v", err)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}
