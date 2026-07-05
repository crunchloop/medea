package creds

import (
	"context"
	"fmt"

	onepassword "github.com/1password/onepassword-sdk-go"
)

// opSDKVault is the concrete SecretVault backed by the 1Password Go SDK (service
// account token). This is the ACL edge — the only file importing the SDK, like
// internal/talos/k8supgrade quarantines the Talos main module. It is not
// unit-tested (it talks to 1Password's servers); the fake in onepassword_test.go
// covers OnePasswordStore's logic, and a gated integration job exercises this
// (design/credentials.md §10).
type opSDKVault struct {
	client *onepassword.Client
}

// NewOnePasswordSDKVault builds a SecretVault from a service-account token — no
// `op` binary needed. NOTE: the SDK requires CGO to compile (its !cgo build is a
// hard error on linux/darwin) and its desktop-app path uses import "C", so the
// binary links libc: the image builds CGO_ENABLED=1 on distroless/base, not
// static (see deploy/Dockerfile, design/credentials.md §4.2).
func NewOnePasswordSDKVault(ctx context.Context, serviceAccountToken string) (SecretVault, error) {
	c, err := onepassword.NewClient(ctx,
		onepassword.WithServiceAccountToken(serviceAccountToken),
		onepassword.WithIntegrationInfo("Medea", "v1"),
	)
	if err != nil {
		return nil, fmt.Errorf("1Password client: %w", err)
	}
	return &opSDKVault{client: c}, nil
}

// ReadField resolves a single field via an op:// reference — the cheap read path.
func (o *opSDKVault) ReadField(ctx context.Context, vault, item, field string) ([]byte, error) {
	ref := fmt.Sprintf("op://%s/%s/%s", vault, item, field)
	val, err := o.client.Secrets().Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", ref, err)
	}
	return []byte(val), nil
}

// WriteFields upserts the per-cluster item, setting only the named fields and
// leaving the rest intact (so PutSecrets doesn't wipe talosconfig/kubeconfig).
func (o *opSDKVault) WriteFields(ctx context.Context, vault, item string, fields map[string][]byte) error {
	vaultID, err := o.vaultID(ctx, vault)
	if err != nil {
		return err
	}
	overview, err := o.findItem(ctx, vaultID, item)
	if err != nil {
		return err
	}
	if overview == nil {
		created := make([]onepassword.ItemField, 0, len(fields))
		for name, val := range fields {
			created = append(created, concealedField(name, string(val)))
		}
		_, err := o.client.Items().Create(ctx, onepassword.ItemCreateParams{
			Category: onepassword.ItemCategorySecureNote,
			VaultID:  vaultID,
			Title:    item,
			Fields:   created,
		})
		if err != nil {
			return fmt.Errorf("create 1Password item %q: %w", item, err)
		}
		return nil
	}

	full, err := o.client.Items().Get(ctx, vaultID, overview.ID)
	if err != nil {
		return fmt.Errorf("get 1Password item %q: %w", item, err)
	}
	for name, val := range fields {
		setField(&full, name, string(val))
	}
	if _, err := o.client.Items().Put(ctx, full); err != nil {
		return fmt.Errorf("update 1Password item %q: %w", item, err)
	}
	return nil
}

// DeleteItem removes the per-cluster item entirely. A missing item (or vault) is
// treated as already-deleted so teardown is idempotent.
func (o *opSDKVault) DeleteItem(ctx context.Context, vault, item string) error {
	vaultID, err := o.vaultID(ctx, vault)
	if err != nil {
		return err
	}
	overview, err := o.findItem(ctx, vaultID, item)
	if err != nil {
		return err
	}
	if overview == nil {
		return nil
	}
	if err := o.client.Items().Delete(ctx, vaultID, overview.ID); err != nil {
		return fmt.Errorf("delete 1Password item %q: %w", item, err)
	}
	return nil
}

func (o *opSDKVault) vaultID(ctx context.Context, name string) (string, error) {
	vaults, err := o.client.Vaults().List(ctx)
	if err != nil {
		return "", fmt.Errorf("list 1Password vaults: %w", err)
	}
	for _, v := range vaults {
		if v.Title == name {
			return v.ID, nil
		}
	}
	return "", fmt.Errorf("1Password vault %q not found (does the service account have access?)", name)
}

func (o *opSDKVault) findItem(ctx context.Context, vaultID, title string) (*onepassword.ItemOverview, error) {
	items, err := o.client.Items().List(ctx, vaultID)
	if err != nil {
		return nil, fmt.Errorf("list 1Password items: %w", err)
	}
	for i := range items {
		if items[i].Title == title {
			return &items[i], nil
		}
	}
	return nil, nil
}

// setField updates the value of an existing field (matched by title) or appends
// a new concealed field.
func setField(item *onepassword.Item, name, value string) {
	for i := range item.Fields {
		if item.Fields[i].Title == name {
			item.Fields[i].Value = value
			return
		}
	}
	item.Fields = append(item.Fields, concealedField(name, value))
}

func concealedField(name, value string) onepassword.ItemField {
	return onepassword.ItemField{
		ID:        name,
		Title:     name,
		FieldType: onepassword.ItemFieldTypeConcealed,
		Value:     value,
	}
}
