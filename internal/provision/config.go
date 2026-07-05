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

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
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

// GenerateSecrets mints a NEW cluster machine-secrets bundle (fresh CAs, machine
// and bootstrap tokens, cluster id/secret) for creating a NEW cluster
// (design/cluster-bootstrap.md). This is the inverse of talos.CaptureSecrets,
// which reuses an EXISTING cluster's bundle to join it. The caller must persist
// the result to the CredentialStore (Medea owns the PKI from t=0) before
// rendering configs, and must not regenerate for a cluster that already has one.
func GenerateSecrets() (*secrets.Bundle, error) {
	b, err := secrets.NewBundle(secrets.NewClock(), config.TalosVersionCurrent)
	if err != nil {
		return nil, fmt.Errorf("provision: generate secrets bundle: %w", err)
	}
	return b, nil
}

// ControlPlaneConfigInput is everything needed to render the FIRST control-plane
// machine config of a new cluster (design/cluster-bootstrap.md §5). Single-CP;
// HA (multiple CP members) is future work.
type ControlPlaneConfigInput struct {
	ClusterName          string          // the Talos cluster name
	ControlPlaneEndpoint string          // e.g. "https://192.168.14.160:6443" (pinned before boot)
	KubernetesVersion    string          // e.g. "v1.36.1"
	InstallDisk          string          // e.g. "/dev/nvme0n1"
	InstallImage         string          // schematic-derived installer image (with extensions)
	Secrets              *secrets.Bundle // the GENERATED bundle (GenerateSecrets)
	// AllowSchedulingOnControlPlanes lets the single CP run workloads (the homelab
	// case). Redundant if a patch already sets it, but explicit here.
	AllowSchedulingOnControlPlanes bool
	// Patches are optional gen-config patches applied on top (the talos/patches/*
	// layer: CNI-none + the inline-Cilium manifest, kube-proxy off, etc.). Strategic
	// merge or JSON6902, same as `talosctl gen config --config-patch`.
	Patches [][]byte
}

// RenderControlPlaneConfig produces the first control-plane machine config (YAML
// bytes) for a NEW cluster, from a generated secrets bundle + optional patches.
// The bytes contain secret material and must only ever be written to the node
// (via Matchbox over the LAN), never to bbolt or the export.
func RenderControlPlaneConfig(in ControlPlaneConfigInput) ([]byte, error) {
	if in.Secrets == nil {
		return nil, fmt.Errorf("provision: secrets bundle required")
	}
	if in.ClusterName == "" || in.ControlPlaneEndpoint == "" {
		return nil, fmt.Errorf("provision: cluster name and control-plane endpoint required")
	}

	opts := []generate.Option{
		generate.WithSecretsBundle(in.Secrets),
		generate.WithAllowSchedulingOnControlPlanes(in.AllowSchedulingOnControlPlanes),
	}
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
	prov, err := input.Config(machine.TypeControlPlane)
	if err != nil {
		return nil, fmt.Errorf("provision: render control-plane config: %w", err)
	}
	out, err := prov.EncodeBytes()
	if err != nil {
		return nil, err
	}
	if len(in.Patches) == 0 {
		return out, nil
	}
	return applyPatches(out, in.Patches)
}

// applyPatches layers gen-config patches (strategic-merge or JSON6902) onto an
// encoded machine config — the talos/patches/* the homelab bakes in at gen time.
func applyPatches(configBytes []byte, patches [][]byte) ([]byte, error) {
	loaded := make([]configpatcher.Patch, 0, len(patches))
	for i, p := range patches {
		patch, err := configpatcher.LoadPatch(p)
		if err != nil {
			return nil, fmt.Errorf("provision: load patch %d: %w", i, err)
		}
		loaded = append(loaded, patch)
	}
	out, err := configpatcher.Apply(configpatcher.WithBytes(configBytes), loaded)
	if err != nil {
		return nil, fmt.Errorf("provision: apply patches: %w", err)
	}
	return out.Bytes()
}
