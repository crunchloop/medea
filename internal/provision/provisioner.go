package provision

import "context"

// Provisioner stages a host's network-boot configuration so that, on its next
// PXE boot, it installs Talos with the given machine config and joins the
// cluster — and unstages it on release (design/provisioning-plane.md §3). It is
// the ACL seam over the boot infrastructure (Matchbox today); reconcilers depend
// on this interface, not on the concrete driver, so they unit-test with a fake.
type Provisioner interface {
	// Stage publishes the boot Profile + machine config for a host (keyed by
	// MAC). Idempotent: re-staging overwrites.
	Stage(ctx context.Context, mac string, p Profile, machineConfig []byte) error
	// Unstage removes a host's staged boot config (release / replacement).
	Unstage(ctx context.Context, mac string) error
}

// Profile is the network-boot configuration for a host: the kernel/initrd assets
// (from the resolved Image Factory schematic, §6) and extra kernel cmdline args.
// The driver wires the machine config in via the boot infra's own mechanism
// (Matchbox generic config), so callers need not put a config URL in Args.
type Profile struct {
	Kernel string   // kernel image URL (schematic boot asset)
	Initrd []string // initrd image URLs
	Args   []string // extra kernel cmdline args (e.g. console=, talos.platform=metal)
}
