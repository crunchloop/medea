package talos

import "context"

// K8sUpgrader is the single seam over Talos's main-module `upgrade-k8s`
// orchestration — the one place Medea depends on Talos's heavy main module
// (PRD §13 #15, §8.4; design/talos-client.md §1, §4). The rollout reconciler
// depends on this interface, never on the dependency itself, so the version
// coupling and any breaking upstream change are contained to the single
// implementing package, internal/talos/k8supgrade.
//
// Unlike the OS path (Client.UpgradeOS, which Medea drives node-by-node),
// upgrade-k8s is cluster-orchestrated by Talos itself: it sequences
// control-plane static-pod manifests and rolls kubelets, with no A/B image swap
// or node reboot (PRD §8.3). The implementation triggers it for a from→to
// version path and returns when Talos reports it done; the caller observes
// convergence by polling kubelet versions via the kube client
// (design/rollout-controller.md §2.2). See design/aggregates/cluster-rollout.md.
type K8sUpgrader interface {
	// UpgradeK8s triggers Talos's Kubernetes upgrade from one version to
	// another and returns when the orchestration completes or fails.
	UpgradeK8s(ctx context.Context, from, to string) error
}
