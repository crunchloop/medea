// Package k8supgrade is the quarantined home of Talos's main-module
// `upgrade-k8s` orchestration (design/talos-client.md §4, §7; PRD §13 #15,
// §8.4). It is the ONLY package permitted to import the Talos *main* module
// (github.com/siderolabs/talos/pkg/cluster + its kubernetes upgrade
// subpackage). Everything else depends on the talos.K8sUpgrader interface, so
// the heavy, version-coupled dependency — and any breaking upstream change —
// touches exactly this package and no other.
//
// STATUS: scaffold (M3 not yet landed). The upstream wiring — constructing the
// cluster provider from talosconfig+endpoints and driving k8s.Upgrade with a
// from→to path, pinned against a supported Talos release — is the M3 work.
// UpgradeK8s currently returns ErrNotImplemented so the seam, the constructor,
// and the integration-test slot (internal/itest/k8s_upgrade_integration_test.go)
// all exist *without* pulling the main-module dependency tree before we commit
// to the version pin. Implementing it is a change confined to this file.
package k8supgrade

import (
	"context"
	"errors"

	"github.com/bilby91/medea/internal/talos"
)

// ErrNotImplemented is returned by UpgradeK8s until the M3 upstream wiring lands.
var ErrNotImplemented = errors.New("k8supgrade: upgrade-k8s not implemented yet (M3)")

// Upgrader implements talos.K8sUpgrader over Talos's main-module orchestration.
// It is built from the same inputs as talos.New: a cluster's talosconfig and its
// control-plane endpoints (design/talos-client.md §2).
type Upgrader struct {
	talosconfig []byte
	endpoints   []string
}

// New builds an Upgrader for a cluster. It does no I/O — the connection to Talos
// is established lazily when UpgradeK8s runs (M3).
func New(talosconfig []byte, endpoints []string) (*Upgrader, error) {
	if len(talosconfig) == 0 {
		return nil, errors.New("k8supgrade: empty talosconfig")
	}
	if len(endpoints) == 0 {
		return nil, errors.New("k8supgrade: no control-plane endpoints")
	}
	return &Upgrader{talosconfig: talosconfig, endpoints: endpoints}, nil
}

// UpgradeK8s triggers Talos's cluster-orchestrated Kubernetes upgrade and
// returns when it completes or fails. See the package doc for status.
//
// M3 implementation outline (design/talos-client.md §4, rollout-controller.md
// §2.2): build the upstream cluster provider from u.talosconfig + u.endpoints,
// call Talos's upgrade-k8s with {from, to}, and let it sequence control-plane
// components and kubelets. Talos drives the disruption; the caller observes
// convergence by polling kubelet versions via the kube client. The from→to pair
// must be a path the pinned Talos release supports.
func (u *Upgrader) UpgradeK8s(ctx context.Context, from, to string) error {
	_ = ctx
	_ = from
	_ = to
	return ErrNotImplemented
}

// Compile-time check that Upgrader satisfies the published seam.
var _ talos.K8sUpgrader = (*Upgrader)(nil)
