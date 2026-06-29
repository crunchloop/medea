package matchbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bilby91/medea/internal/provision"
)

func TestStageWritesGroupProfileAndConfig(t *testing.T) {
	root := t.TempDir()
	s, err := New(root, "http://matchbox:8086/")
	if err != nil {
		t.Fatal(err)
	}
	cfg := []byte("machine:\n  type: worker\n")
	err = s.Stage(context.Background(), "AA:BB:CC:DD:EE:FF", provision.Profile{
		Kernel: "https://factory/kernel",
		Initrd: []string{"https://factory/initrd"},
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

	// profile carries boot assets + references the generic config via generic_id.
	var p profile
	readJSON(t, filepath.Join(root, profilesDir, key+".json"), &p)
	if p.Boot.Kernel != "https://factory/kernel" || len(p.Boot.Initrd) != 1 || p.GenericID != key {
		t.Fatalf("profile wrong: %+v", p)
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
