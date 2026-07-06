package creds

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

// TestOnePasswordSmoke is a LIVE round-trip against a real 1Password vault via a
// service-account token — the runtime proof that the SDK works without CGO (run
// it with CGO_ENABLED=0). It is skipped unless both env vars are set, so it never
// runs in normal CI:
//
//	OP_SERVICE_ACCOUNT_TOKEN=$(cat /path/to/token) \
//	OP_SMOKE_VAULT=<vault> \
//	  CGO_ENABLED=0 go test -run TestOnePasswordSmoke ./internal/creds/ -v
//
// It writes, reads back, and DELETES a throwaway item named for cluster
// "medea-smoketest" — safe to run against a real vault.
func TestOnePasswordSmoke(t *testing.T) {
	token := os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")
	vaultName := os.Getenv("OP_SMOKE_VAULT")
	if token == "" || vaultName == "" {
		t.Skip("OP_SERVICE_ACCOUNT_TOKEN and OP_SMOKE_VAULT not set; skipping live 1Password smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	vault, err := NewOnePasswordSDKVault(ctx, token)
	if err != nil {
		t.Fatalf("NewOnePasswordSDKVault (client init runs the WASM core — this is the CGO-free runtime check): %v", err)
	}
	store := NewOnePasswordStore(vaultName, vault)

	const cluster = "medea-smoketest"
	// Clean slate + guaranteed cleanup — never leave a test item behind.
	_ = store.Delete(cluster)
	t.Cleanup(func() { _ = store.Delete(cluster) })

	talos := []byte("TALOS-SMOKE")
	kube := []byte("KUBE-SMOKE")
	secrets := []byte("SECRETS-SMOKE")

	if err := store.Put(cluster, talos, kube); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, err := store.TalosConfig(cluster); err != nil || !bytes.Equal(got, talos) {
		t.Fatalf("TalosConfig round-trip: got %q err %v", got, err)
	}
	if got, err := store.KubeConfig(cluster); err != nil || !bytes.Equal(got, kube) {
		t.Fatalf("KubeConfig round-trip: got %q err %v", got, err)
	}

	// PutSecrets must add secrets without clobbering talos/kube on the same item.
	if err := store.PutSecrets(cluster, secrets); err != nil {
		t.Fatalf("PutSecrets: %v", err)
	}
	if got, err := store.Secrets(cluster); err != nil || !bytes.Equal(got, secrets) {
		t.Fatalf("Secrets round-trip: got %q err %v", got, err)
	}
	if got, err := store.TalosConfig(cluster); err != nil || !bytes.Equal(got, talos) {
		t.Fatalf("talosconfig clobbered by PutSecrets: got %q err %v", got, err)
	}

	// Delete removes the whole item; reads then fail.
	if err := store.Delete(cluster); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.TalosConfig(cluster); err == nil {
		t.Fatal("expected error reading talosconfig after delete")
	}

	t.Log("1Password service-account round-trip OK — SDK works CGO-free at runtime")
}
