package creds

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// op:// references are slash-delimited (op://vault/item/field), so the per-cluster
// item title must not contain a slash — else Secrets().Resolve can't address it.
// Regression: the first cut used "medea/<cluster>", which wrote fine but was
// unreadable.
func TestItemTitleHasNoSlash(t *testing.T) {
	if got := itemTitle("dap"); strings.Contains(got, "/") {
		t.Fatalf("item title %q contains a slash — unresolvable via op://", got)
	}
}

// fakeVault is an in-memory SecretVault: vault/item -> field -> value, with the
// merge (non-clobbering) WriteFields semantics the real impl must honor.
type fakeVault struct {
	items map[string]map[string][]byte
}

func newFakeVault() *fakeVault { return &fakeVault{items: map[string]map[string][]byte{}} }

func (f *fakeVault) key(vault, item string) string { return vault + "/" + item }

func (f *fakeVault) ReadField(_ context.Context, vault, item, field string) ([]byte, error) {
	it, ok := f.items[f.key(vault, item)]
	if !ok {
		return nil, fmt.Errorf("no item %q in vault %q", item, vault)
	}
	v, ok := it[field]
	if !ok {
		return nil, fmt.Errorf("no field %q in item %q", field, item)
	}
	return v, nil
}

func (f *fakeVault) WriteFields(_ context.Context, vault, item string, fields map[string][]byte) error {
	k := f.key(vault, item)
	if f.items[k] == nil {
		f.items[k] = map[string][]byte{}
	}
	for name, val := range fields {
		f.items[k][name] = val
	}
	return nil
}

func TestOnePasswordStoreRoundTrip(t *testing.T) {
	s := NewOnePasswordStore("Kubernetes", newFakeVault())

	if err := s.Put("home", []byte("TALOS"), []byte("KUBE")); err != nil {
		t.Fatal(err)
	}
	if tc, err := s.TalosConfig("home"); err != nil || string(tc) != "TALOS" {
		t.Fatalf("talos: %v / %q", err, tc)
	}
	if kc, err := s.KubeConfig("home"); err != nil || string(kc) != "KUBE" {
		t.Fatalf("kube: %v / %q", err, kc)
	}

	if err := s.PutSecrets("home", []byte("SECRETS-BUNDLE")); err != nil {
		t.Fatal(err)
	}
	if sec, err := s.Secrets("home"); err != nil || string(sec) != "SECRETS-BUNDLE" {
		t.Fatalf("secrets: %v / %q", err, sec)
	}

	if _, err := s.TalosConfig("missing"); err == nil {
		t.Fatal("expected error for missing cluster")
	}
}

// PutSecrets must not clobber talosconfig/kubeconfig on the same item — the two
// writers touch different fields of one cluster item.
func TestOnePasswordPutSecretsPreservesCreds(t *testing.T) {
	s := NewOnePasswordStore("Kubernetes", newFakeVault())
	if err := s.Put("home", []byte("TALOS"), []byte("KUBE")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecrets("home", []byte("SECRETS")); err != nil {
		t.Fatal(err)
	}
	if tc, err := s.TalosConfig("home"); err != nil || string(tc) != "TALOS" {
		t.Fatalf("talosconfig lost after PutSecrets: %v / %q", err, tc)
	}
	if kc, err := s.KubeConfig("home"); err != nil || string(kc) != "KUBE" {
		t.Fatalf("kubeconfig lost after PutSecrets: %v / %q", err, kc)
	}
}

func TestMigrateFileToOnePassword(t *testing.T) {
	from, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := from.Put("home", []byte("TALOS"), []byte("KUBE")); err != nil {
		t.Fatal(err)
	}
	if err := from.PutSecrets("home", []byte("SECRETS")); err != nil {
		t.Fatal(err)
	}

	to := NewOnePasswordStore("Kubernetes", newFakeVault())
	if err := Migrate(from, to, "home"); err != nil {
		t.Fatal(err)
	}

	for field, want := range map[string]func() ([]byte, error){
		"TALOS":   func() ([]byte, error) { return to.TalosConfig("home") },
		"KUBE":    func() ([]byte, error) { return to.KubeConfig("home") },
		"SECRETS": func() ([]byte, error) { return to.Secrets("home") },
	} {
		got, err := want()
		if err != nil || string(got) != field {
			t.Fatalf("migrated field %q: %v / %q", field, err, got)
		}
	}
}

// Migration must not fail when a cluster has no captured secrets bundle yet.
func TestMigrateSkipsMissingSecrets(t *testing.T) {
	from, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := from.Put("home", []byte("TALOS"), []byte("KUBE")); err != nil {
		t.Fatal(err)
	}

	to := NewOnePasswordStore("Kubernetes", newFakeVault())
	if err := Migrate(from, to, "home"); err != nil {
		t.Fatalf("migrate without secrets should succeed: %v", err)
	}
	if _, err := to.Secrets("home"); err == nil {
		t.Fatal("expected no secrets present after migrating a cluster that had none")
	}
}
