// Package creds stores per-cluster credentials (talosconfig, kubeconfig) that
// Medea uses to reach managed clusters. These are sensitive and deliberately
// kept OUT of the bbolt resource store (design/api-and-auth.md §5,
// datastore.md §9) so the desired-state export stays safe to inspect.
package creds

import (
	"fmt"
	"os"
	"path/filepath"
)

// Store resolves credentials by cluster name. The bbolt store references a
// cluster only by name; the secret material lives here.
type Store interface {
	TalosConfig(cluster string) ([]byte, error)
	KubeConfig(cluster string) ([]byte, error)
	Put(cluster string, talos, kube []byte) error
	// Secrets is the cluster machine-secrets bundle (Talos secrets.yaml),
	// captured from the live cluster for provisioning join configs
	// (design/provisioning-plane.md §5).
	Secrets(cluster string) ([]byte, error)
	PutSecrets(cluster string, secrets []byte) error
}

// FileStore is the v1 implementation: a 0700 directory of per-cluster 0600
// files. A 1Password-backed Store is the planned v2 (api-and-auth.md §5).
type FileStore struct {
	dir string
}

const (
	talosFile   = "talosconfig"
	kubeFile    = "kubeconfig"
	secretsFile = "secrets.yaml"
)

// NewFileStore roots a FileStore at dir, creating it 0700 if needed.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creds dir: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

func (f *FileStore) TalosConfig(cluster string) ([]byte, error) {
	return f.read(cluster, talosFile)
}

func (f *FileStore) KubeConfig(cluster string) ([]byte, error) {
	return f.read(cluster, kubeFile)
}

func (f *FileStore) Put(cluster string, talos, kube []byte) error {
	dir := filepath.Join(f.dir, cluster)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeSecret(filepath.Join(dir, talosFile), talos); err != nil {
		return err
	}
	return writeSecret(filepath.Join(dir, kubeFile), kube)
}

func (f *FileStore) Secrets(cluster string) ([]byte, error) {
	return f.read(cluster, secretsFile)
}

func (f *FileStore) PutSecrets(cluster string, secrets []byte) error {
	dir := filepath.Join(f.dir, cluster)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeSecret(filepath.Join(dir, secretsFile), secrets)
}

func (f *FileStore) read(cluster, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(f.dir, cluster, name))
	if err != nil {
		return nil, fmt.Errorf("read %s for %q: %w", name, cluster, err)
	}
	return b, nil
}

func writeSecret(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
