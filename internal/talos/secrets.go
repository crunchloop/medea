package talos

import (
	"context"
	"fmt"

	"github.com/cosi-project/runtime/pkg/resource"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	configres "github.com/siderolabs/talos/pkg/machinery/resources/config"
	"gopkg.in/yaml.v3"
)

// CaptureSecrets reads node's active machine config and extracts the cluster's
// machine-secrets bundle (CA, tokens, cluster id/secret) as Talos secrets.yaml
// bytes — the material needed to mint join configs for new nodes
// (design/provisioning-plane.md §5).
//
// It does NOT generate new secrets (that would mint a *different* cluster); it
// captures the EXISTING cluster's, via secrets.NewBundleFromConfig over the live
// config. Read-only against the cluster.
func (t *Client) CaptureSecrets(ctx context.Context, node string) ([]byte, error) {
	md := resource.NewMetadata(configres.NamespaceName, configres.MachineConfigType, configres.ActiveID, resource.VersionUndefined)
	// COSI Get is strictly one-to-one — WithNode (singular), as in image.go.
	res, err := t.c.COSI.Get(talosclient.WithNode(ctx, node), md)
	if err != nil {
		return nil, fmt.Errorf("read machine config from %s: %w", node, err)
	}
	mc, ok := res.(*configres.MachineConfig)
	if !ok {
		return nil, fmt.Errorf("talos: unexpected resource type %T", res)
	}
	prov := mc.Provider()
	if prov == nil {
		return nil, fmt.Errorf("talos: node %s has no machine config", node)
	}
	bundle := secrets.NewBundleFromConfig(secrets.NewClock(), prov)
	out, err := yaml.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("marshal secrets bundle: %w", err)
	}
	return out, nil
}
