package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultFactoryHost is the public Talos Image Factory.
const DefaultFactoryHost = "factory.talos.dev"

// Resolver turns a pool's system-extension set into a pinned Image Factory
// schematic ID (design/provisioning-plane.md §6). The ID then derives the boot
// assets (for PXE) and the install image. Behind an interface so the
// provisioning reconciler unit-tests with a fake.
type Resolver interface {
	// Resolve returns the schematic ID for the given official extensions
	// (empty = the stock, no-extensions schematic).
	Resolve(ctx context.Context, extensions []string) (string, error)
}

// FactoryClient resolves schematics against an Image Factory HTTP API.
type FactoryClient struct {
	baseURL string // e.g. "https://factory.talos.dev"
	http    *http.Client
}

// NewFactoryClient builds a client against host (default DefaultFactoryHost).
func NewFactoryClient(host string) *FactoryClient {
	if host == "" {
		host = DefaultFactoryHost
	}
	return &FactoryClient{baseURL: "https://" + host, http: &http.Client{Timeout: 30 * time.Second}}
}

// Resolve POSTs a schematic to the factory and returns its content-addressed ID.
// The factory is idempotent: the same customization always yields the same ID,
// so this both creates and looks up.
func (c *FactoryClient) Resolve(ctx context.Context, extensions []string) (string, error) {
	body := schematicYAML(extensions)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/schematics", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/yaml")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("image factory: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("image factory: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("image factory: decode response: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("image factory: empty schematic id")
	}
	return out.ID, nil
}

// schematicYAML builds the Image Factory schematic document. Empty extensions
// produce the stock (no-extensions) schematic.
func schematicYAML(extensions []string) string {
	if len(extensions) == 0 {
		return "customization: {}\n"
	}
	var b bytes.Buffer
	b.WriteString("customization:\n  systemExtensions:\n    officialExtensions:\n")
	for _, e := range extensions {
		fmt.Fprintf(&b, "      - %s\n", e)
	}
	return b.String()
}

// InstallImage returns the Talos installer image for a schematic + version,
// matching the factory.talos.dev/metal-installer/<id>:<version> shape the v1
// rollout already preserves (talos-client.md §3).
func InstallImage(host, schematicID, version string) string {
	if host == "" {
		host = DefaultFactoryHost
	}
	return fmt.Sprintf("%s/metal-installer/%s:%s", host, schematicID, version)
}

// BootAssets returns the PXE kernel + initrd URLs for a schematic + version.
func BootAssets(host, schematicID, version, arch string) (kernel string, initrd []string) {
	if host == "" {
		host = DefaultFactoryHost
	}
	if arch == "" {
		arch = "amd64"
	}
	base := fmt.Sprintf("https://%s/image/%s/%s", host, schematicID, version)
	return base + "/kernel-" + arch, []string{base + "/initramfs-" + arch + ".xz"}
}

// compile-time check that FactoryClient satisfies the seam.
var _ Resolver = (*FactoryClient)(nil)
