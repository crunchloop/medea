package creds

import (
	"context"
	"fmt"
	"time"
)

// SecretVault is the minimal 1Password surface OnePasswordStore needs: read one
// field, and create-or-update selected fields of an item *without clobbering the
// fields it does not name*. It is the ACL seam — the concrete impl (1Password Go
// SDK / Connect) is the only place that imports an external client, and tests use
// a fake. Items are addressed by title within a vault.
type SecretVault interface {
	ReadField(ctx context.Context, vault, item, field string) ([]byte, error)
	WriteFields(ctx context.Context, vault, item string, fields map[string][]byte) error
}

// OnePasswordStore is a creds.Store backed by a 1Password vault: one item per
// cluster (titled "medea/<cluster>") with talosconfig / kubeconfig / secrets.yaml
// fields. Keeps credentials off the Medea host disk (design/credentials.md §4).
// Like the file store, this material never enters bbolt or the desired-state
// Export — the vault is the store, not a serialized export.
type OnePasswordStore struct {
	vaultName string
	vault     SecretVault
	timeout   time.Duration
}

// opTimeout bounds each 1Password call so a slow/unreachable vault can't wedge a
// synchronous creds.Store method (the interface is context-free by design).
const opTimeout = 30 * time.Second

// NewOnePasswordStore returns a creds.Store over the given vault seam.
func NewOnePasswordStore(vaultName string, v SecretVault) *OnePasswordStore {
	return &OnePasswordStore{vaultName: vaultName, vault: v, timeout: opTimeout}
}

// itemTitle is the per-cluster 1Password item title. Namespaced so a shared vault
// (the cluster's existing DR vault) doesn't collide with unrelated items. The
// separator is a hyphen, NOT a slash: op:// secret references are slash-delimited
// (op://vault/item/field), so a slash in the title makes the item unresolvable.
func itemTitle(cluster string) string { return "medea-" + cluster }

func (o *OnePasswordStore) withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), o.timeout)
}

func (o *OnePasswordStore) read(cluster, field string) ([]byte, error) {
	ctx, cancel := o.withTimeout()
	defer cancel()
	b, err := o.vault.ReadField(ctx, o.vaultName, itemTitle(cluster), field)
	if err != nil {
		return nil, fmt.Errorf("read %s for %q from 1Password: %w", field, cluster, err)
	}
	return b, nil
}

func (o *OnePasswordStore) TalosConfig(cluster string) ([]byte, error) {
	return o.read(cluster, talosFile)
}

func (o *OnePasswordStore) KubeConfig(cluster string) ([]byte, error) {
	return o.read(cluster, kubeFile)
}

func (o *OnePasswordStore) Secrets(cluster string) ([]byte, error) {
	return o.read(cluster, secretsFile)
}

func (o *OnePasswordStore) Put(cluster string, talos, kube []byte) error {
	ctx, cancel := o.withTimeout()
	defer cancel()
	if err := o.vault.WriteFields(ctx, o.vaultName, itemTitle(cluster), map[string][]byte{
		talosFile: talos,
		kubeFile:  kube,
	}); err != nil {
		return fmt.Errorf("write creds for %q to 1Password: %w", cluster, err)
	}
	return nil
}

func (o *OnePasswordStore) PutSecrets(cluster string, secrets []byte) error {
	ctx, cancel := o.withTimeout()
	defer cancel()
	// Only the secrets field — WriteFields must preserve talosconfig/kubeconfig.
	if err := o.vault.WriteFields(ctx, o.vaultName, itemTitle(cluster), map[string][]byte{
		secretsFile: secrets,
	}); err != nil {
		return fmt.Errorf("write secrets for %q to 1Password: %w", cluster, err)
	}
	return nil
}

// Static assertions: both impls satisfy the Store interface.
var (
	_ Store = (*OnePasswordStore)(nil)
	_ Store = (*FileStore)(nil)
)

// Migrate copies a cluster's credentials (talosconfig, kubeconfig, and the
// secrets bundle if present) from one store to another — used to move off the
// file backend onto 1Password so home-cluster's _out/ can be retired
// (design/credentials.md §4.1). The secrets bundle is optional (only present
// after `capture-secrets`); its absence is skipped, not fatal.
func Migrate(from, to Store, cluster string) error {
	talos, err := from.TalosConfig(cluster)
	if err != nil {
		return fmt.Errorf("migrate: read talosconfig: %w", err)
	}
	kube, err := from.KubeConfig(cluster)
	if err != nil {
		return fmt.Errorf("migrate: read kubeconfig: %w", err)
	}
	if err := to.Put(cluster, talos, kube); err != nil {
		return fmt.Errorf("migrate: write creds: %w", err)
	}
	if secrets, err := from.Secrets(cluster); err == nil {
		if err := to.PutSecrets(cluster, secrets); err != nil {
			return fmt.Errorf("migrate: write secrets: %w", err)
		}
	}
	return nil
}
