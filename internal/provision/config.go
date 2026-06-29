// Package provision is the v2 provisioning plane: turning bare metal into cluster
// members (design/provisioning-plane.md). This file renders Talos machine
// configs for joining nodes from a cluster/pool spec + the captured cluster
// secrets bundle (provisioning-plane.md §5); sibling files hold the Matchbox
// driver (the Provisioner seam) and Image-Factory schematic resolution.
//
// v2-M2 ships these building blocks, unit-tested with fakes; the reconciler that
// drives them lands in v2-M3.
package provision

import (
	"fmt"

	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"gopkg.in/yaml.v3"
)

// LoadSecretsBundle parses captured secrets.yaml bytes (creds.Secrets, M1) into a
// bundle usable by RenderWorkerConfig. It restores the Clock (yaml-skipped) so
// the bundle is fully usable.
func LoadSecretsBundle(b []byte) (*secrets.Bundle, error) {
	var bundle secrets.Bundle
	if err := yaml.Unmarshal(b, &bundle); err != nil {
		return nil, fmt.Errorf("provision: parse secrets bundle: %w", err)
	}
	bundle.Clock = secrets.NewClock()
	return &bundle, nil
}

// WorkerConfigInput is everything needed to render a worker join config. v2
// provisions workers into an existing cluster (provisioning-plane.md §4, §9);
// control-plane joins (HA) are future work.
type WorkerConfigInput struct {
	ClusterName          string          // the Talos cluster name
	ControlPlaneEndpoint string          // e.g. "https://10.5.0.2:6443"
	KubernetesVersion    string          // e.g. "v1.36.2"
	InstallDisk          string          // e.g. "/dev/sda" (uniform on the Beelinks)
	InstallImage         string          // schematic-derived installer image (provisioning-plane.md §6)
	Secrets              *secrets.Bundle // the EXISTING cluster's secrets (captured, M1)
}

// RenderWorkerConfig produces a Talos worker machine config (YAML bytes) that
// joins the existing cluster. It uses the captured secrets bundle rather than
// generating new secrets — so the node trusts and is trusted by the running
// cluster. The bytes contain secret material and must only ever be written to
// the node (via Matchbox over the LAN), never to bbolt or the export.
func RenderWorkerConfig(in WorkerConfigInput) ([]byte, error) {
	if in.Secrets == nil {
		return nil, fmt.Errorf("provision: secrets bundle required")
	}
	if in.ClusterName == "" || in.ControlPlaneEndpoint == "" {
		return nil, fmt.Errorf("provision: cluster name and control-plane endpoint required")
	}

	opts := []generate.Option{generate.WithSecretsBundle(in.Secrets)}
	if in.InstallDisk != "" {
		opts = append(opts, generate.WithInstallDisk(in.InstallDisk))
	}
	if in.InstallImage != "" {
		opts = append(opts, generate.WithInstallImage(in.InstallImage))
	}

	input, err := generate.NewInput(in.ClusterName, in.ControlPlaneEndpoint, in.KubernetesVersion, opts...)
	if err != nil {
		return nil, fmt.Errorf("provision: build config input: %w", err)
	}
	prov, err := input.Config(machine.TypeWorker)
	if err != nil {
		return nil, fmt.Errorf("provision: render worker config: %w", err)
	}
	return prov.EncodeBytes()
}
