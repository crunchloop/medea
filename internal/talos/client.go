// Package talos wraps the Talos machine API behind small Medea-owned methods
// (design/talos-client.md). Per PRD §13 #15 Medea imports Talos Go packages
// rather than shelling out to talosctl. This file uses only the lightweight,
// externally-versioned `machinery` module; the version-coupled upgrade-k8s
// import (main module) lands separately in M2 (internal/talos/k8supgrade).
package talos

import (
	"context"
	"fmt"
	"io"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

// Client is a connection to a cluster's Talos machine API.
type Client struct {
	c *talosclient.Client
}

// New builds a Client from talosconfig bytes (resolved from the CredentialStore)
// and optional endpoints (control-plane node IPs the API routes through; if
// empty, the endpoints embedded in the talosconfig are used).
func New(ctx context.Context, talosconfigBytes []byte, endpoints []string) (*Client, error) {
	cfg, err := talosconfig.FromBytes(talosconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("parse talosconfig: %w", err)
	}
	opts := []talosclient.OptionFunc{talosclient.WithConfig(cfg)}
	if len(endpoints) > 0 {
		opts = append(opts, talosclient.WithEndpoints(endpoints...))
	}
	c, err := talosclient.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("talos client: %w", err)
	}
	return &Client{c: c}, nil
}

// Close releases the underlying connection.
func (t *Client) Close() error { return t.c.Close() }

// Version returns the Talos version tag (e.g. "v1.13.5") running on node.
func (t *Client) Version(ctx context.Context, node string) (string, error) {
	resp, err := t.c.Version(talosclient.WithNodes(ctx, node))
	if err != nil {
		return "", err
	}
	msgs := resp.GetMessages()
	if len(msgs) == 0 {
		return "", fmt.Errorf("talos: empty version response from %s", node)
	}
	return msgs[0].GetVersion().GetTag(), nil
}

// UpgradeOS triggers an atomic A/B OS upgrade of node to the given installer
// image; the node reboots into the new image. The image MUST include the node's
// system extensions / Image Factory schematic — the caller derives it
// (talos-client.md §3); a bare installer image silently drops extensions.
//
// DESTRUCTIVE: reboots the node. Only ever invoked by the rollout reconciler
// after drain + (for control-plane) an etcd snapshot.
func (t *Client) UpgradeOS(ctx context.Context, node, image string) error {
	// UpgradeWithOptions is marked deprecated upstream in favor of LifecycleClient,
	// but remains supported and correct for the pinned Talos v1.13; the OS-path is
	// QEMU-validated against it. Migration to LifecycleClient is tracked separately.
	_, err := t.c.UpgradeWithOptions(talosclient.WithNodes(ctx, node), //nolint:staticcheck // SA1019: supported in v1.13; migrate later
		talosclient.WithUpgradeImage(image),
		talosclient.WithUpgradeRebootMode(machineapi.UpgradeRequest_DEFAULT),
	)
	return err
}

// EtcdSnapshot streams an etcd snapshot from a control-plane node into w. Used by
// the rollout's snapshot-before-control-plane gate (rollout-controller.md §3) and,
// later, the backup feature. Non-destructive (a read-only copy of etcd).
func (t *Client) EtcdSnapshot(ctx context.Context, node string, w io.Writer) error {
	rc, err := t.c.EtcdSnapshot(talosclient.WithNodes(ctx, node), &machineapi.EtcdSnapshotRequest{})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = io.Copy(w, rc)
	return err
}
