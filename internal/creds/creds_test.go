package creds

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreRoundTripAndPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds")
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Put("home", []byte("TALOS"), []byte("KUBE")); err != nil {
		t.Fatal(err)
	}

	tc, err := s.TalosConfig("home")
	if err != nil || string(tc) != "TALOS" {
		t.Fatalf("talos: %v / %q", err, tc)
	}
	kc, err := s.KubeConfig("home")
	if err != nil || string(kc) != "KUBE" {
		t.Fatalf("kube: %v / %q", err, kc)
	}

	// dir 0700, secret files 0600
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("creds dir perm = %o, want 700", di.Mode().Perm())
	}
	fi, _ := os.Stat(filepath.Join(dir, "home", talosFile))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("talosconfig perm = %o, want 600", fi.Mode().Perm())
	}

	if _, err := s.TalosConfig("missing"); err == nil {
		t.Fatal("expected error for missing cluster")
	}
}
