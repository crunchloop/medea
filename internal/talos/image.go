package talos

import (
	"context"
	"fmt"
	"strings"

	"github.com/cosi-project/runtime/pkg/resource"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	configres "github.com/siderolabs/talos/pkg/machinery/resources/config"
)

const defaultInstaller = "ghcr.io/siderolabs/installer"

// InstallImage reads a node's current installer image from its machine config
// (machine.install.image). It returns "" when the node has no install section
// (e.g. a docker/container node), letting the caller fall back to the default.
func (t *Client) InstallImage(ctx context.Context, node string) (string, error) {
	md := resource.NewMetadata(configres.NamespaceName, configres.MachineConfigType, configres.ActiveID, resource.VersionUndefined)
	// COSI Get is strictly one-to-one — use WithNode (singular), not WithNodes
	// (plural fan-out), or apid rejects it with "one-2-many proxying not supported".
	res, err := t.c.COSI.Get(talosclient.WithNode(ctx, node), md)
	if err != nil {
		return "", err
	}
	mc, ok := res.(*configres.MachineConfig)
	if !ok {
		return "", fmt.Errorf("talos: unexpected resource type %T", res)
	}
	prov := mc.Provider()
	if prov == nil || prov.Machine() == nil || prov.Machine().Install() == nil {
		return "", nil
	}
	return prov.Machine().Install().Image(), nil
}

// DeriveInstallerImage returns the image to upgrade to: the node's current
// installer image with its tag swapped for targetVersion, preserving the
// registry, repo, and Image Factory schematic (talos-client.md §3) — a bare
// installer image would silently drop the node's system extensions. If current
// is empty it falls back to the default installer image.
func DeriveInstallerImage(current, targetVersion string) string {
	if current == "" {
		return defaultInstaller + ":" + targetVersion
	}
	// The tag is whatever follows the final ':' — unless that ':' is a registry
	// port (its suffix would then contain a '/').
	if i := strings.LastIndex(current, ":"); i >= 0 && !strings.Contains(current[i+1:], "/") {
		return current[:i] + ":" + targetVersion
	}
	return current + ":" + targetVersion
}
