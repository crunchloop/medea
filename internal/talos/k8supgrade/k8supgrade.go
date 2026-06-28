// Package k8supgrade is the quarantined home of Talos's main-module
// `upgrade-k8s` orchestration (design/talos-client.md §4, §7; PRD §13 #15,
// §8.4). It is the ONLY package permitted to import the Talos *main* module
// (github.com/siderolabs/talos/pkg/cluster + its kubernetes upgrade subpackage)
// and the version-coupled github.com/siderolabs/go-kubernetes. Everything else
// depends on the talos.K8sUpgrader interface, so the heavy dependency — and any
// breaking upstream change — touches exactly this package and no other.
//
// The implementation mirrors what `talosctl upgrade-k8s` does internally
// (cmd/talosctl/cmd/talos/upgrade-k8s.go): build a cluster provider from the
// talosconfig + control-plane endpoints, then call kubernetes.Upgrade with a
// from→to path. Talos sequences control-plane static pods and rolls kubelets
// itself (no node reboot, unlike the OS path); Medea triggers and waits.
package k8supgrade

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/siderolabs/go-kubernetes/kubernetes/ssa"
	"github.com/siderolabs/go-kubernetes/kubernetes/upgrade"
	"github.com/siderolabs/talos/pkg/cluster"
	k8s "github.com/siderolabs/talos/pkg/cluster/kubernetes"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/constants"

	"github.com/bilby91/medea/internal/talos"
)

// reconcileTimeout bounds Talos's manifest reconcile step (talosctl's default).
const reconcileTimeout = 5 * time.Minute

// Upgrader implements talos.K8sUpgrader over Talos's main-module orchestration.
// It is built from the same inputs as talos.New: a cluster's talosconfig and its
// control-plane endpoints (design/talos-client.md §2).
type Upgrader struct {
	cfg    *talosconfig.Config
	logOut io.Writer
}

// New builds an Upgrader from a cluster's talosconfig bytes and its
// control-plane endpoints. The endpoints are baked into the talosconfig's active
// context so the cluster provider routes API calls through the control plane.
func New(talosconfigBytes []byte, endpoints []string) (*Upgrader, error) {
	if len(talosconfigBytes) == 0 {
		return nil, fmt.Errorf("k8supgrade: empty talosconfig")
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("k8supgrade: no control-plane endpoints")
	}
	cfg, err := talosconfig.FromBytes(talosconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("k8supgrade: parse talosconfig: %w", err)
	}
	cur := cfg.Contexts[cfg.Context]
	if cur == nil {
		return nil, fmt.Errorf("k8supgrade: talosconfig has no current context %q", cfg.Context)
	}
	cur.Endpoints = endpoints
	return &Upgrader{cfg: cfg, logOut: logBridge{}}, nil
}

// UpgradeK8s triggers Talos's cluster-orchestrated Kubernetes upgrade from one
// version to another and returns when the orchestration completes or fails.
// from/to may carry a leading "v" (NewPath trims it). The component images are
// pinned to the Talos release's enforced defaults (constants.*), matching
// talosctl; an empty image would fail upstream validation.
func (u *Upgrader) UpgradeK8s(ctx context.Context, from, to string) error {
	path, err := upgrade.NewPath(from, to)
	if err != nil {
		return fmt.Errorf("k8supgrade: build upgrade path %s->%s: %w", from, to, err)
	}
	if !path.IsSupported() {
		return fmt.Errorf("k8supgrade: unsupported upgrade path %s", path)
	}

	clientProvider := &cluster.ConfigClientProvider{TalosConfig: u.cfg}
	defer clientProvider.Close() //nolint:errcheck

	provider := struct {
		cluster.ClientProvider
		cluster.K8sProvider
	}{
		ClientProvider: clientProvider,
		K8sProvider:    &cluster.KubernetesClient{ClientProvider: clientProvider},
	}

	opts := k8s.UpgradeOptions{
		Path:                   path,
		UpgradeKubelet:         true,
		PrePullImages:          true,
		KubeletImage:           constants.KubeletImage,
		APIServerImage:         constants.KubernetesAPIServerImage,
		ControllerManagerImage: constants.KubernetesControllerManagerImage,
		SchedulerImage:         constants.KubernetesSchedulerImage,
		ProxyImage:             constants.KubeProxyImage,
		EncoderOpt:             encoder.WithComments(encoder.CommentsDisabled),
		InventoryPolicy:        ssa.InventoryPolicyAdoptIfNoInventory,
		ReconcileTimeout:       reconcileTimeout,
		LogOutput:              u.logOut,
	}

	if err := k8s.Upgrade(ctx, &provider, opts); err != nil {
		return fmt.Errorf("k8supgrade: upgrade-k8s %s: %w", path, err)
	}
	return nil
}

// logBridge routes the upstream upgrade's progress lines to the standard logger
// so an operator watching `medea serve` sees k8s-upgrade progress.
type logBridge struct{}

func (logBridge) Write(p []byte) (int, error) {
	log.Printf("k8supgrade: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// Compile-time check that Upgrader satisfies the published seam.
var _ talos.K8sUpgrader = (*Upgrader)(nil)
