package talos

import (
	"context"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
)

// Bootstrap initializes etcd on a freshly-installed control-plane node — the
// one-time act that turns a lone Talos node into a running cluster
// (design/cluster-bootstrap.md §2).
//
// MUST run EXACTLY ONCE per cluster: running it again re-initializes etcd and
// destroys cluster state. The caller (the bootstrap phase) guards it behind the
// persisted ClusterBootstrap phase so a restart resumes *past* this step.
func (t *Client) Bootstrap(ctx context.Context, node string) error {
	return t.c.Bootstrap(talosclient.WithNodes(ctx, node), &machineapi.BootstrapRequest{})
}

// Kubeconfig fetches the admin kubeconfig from a control-plane node. Valid only
// once etcd is bootstrapped and the apiserver is up; the bootstrap phase calls it
// after AwaitingHealthy and stores the result in the CredentialStore so clients
// get cluster access without _out/.
func (t *Client) Kubeconfig(ctx context.Context, node string) ([]byte, error) {
	return t.c.Kubeconfig(talosclient.WithNodes(ctx, node))
}
