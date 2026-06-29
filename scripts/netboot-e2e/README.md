# Netboot E2E lab

The capstone provisioning test (design/provisioning-plane.md §12): a blank node
PXE-boots via Medea's Matchbox staging, installs Talos, joins a cluster, and
Medea binds it — the full chain nothing below this exercises.

## Cross-platform design

One **Linux-native** harness (`lab.sh`), run via `run.sh`:

- **Linux / CI:** runs directly. With `/dev/kvm` it's fast.
- **macOS:** `run.sh` execs the *same* `lab.sh` inside an **OrbStack** Linux
  machine (`medea-netboot`, created on first use). The repo is visible there at
  the same path. No second networking stack.

Everything runs on one isolated bridge inside the Linux environment, so the host
OS only needs to provide that Linux environment.

**Speed:** Apple Silicon M1/M2 has no nested `/dev/kvm`, so QEMU runs under TCG
(works, slow). M3+/Linux-CI with KVM is fast. `ARCH` defaults to the host arch
(arm64 in the Mac's Linux VM, amd64 in CI) — the provisioning logic under test is
arch-independent; the Beelinks remain the amd64 truth.

## Pieces

- **bridge** `medeabr0` (`10.66.0.1/24`) — the lab L2.
- **dnsmasq** — full DHCP + TFTP + iPXE chainload → Matchbox (isolated bridge, so
  full DHCP, not the proxy-DHCP the real `netboot/` uses alongside a router).
- **Matchbox** — `-data-path` is the dir Medea's driver writes; serves
  `/boot.ipxe`, `/ipxe`, `/generic`.
- **QEMU nodes** — PXE-boot off the bridge (UEFI; amd64 OVMF / arm64 AAVMF).

## Usage

```sh
# from medea/
./scripts/netboot-e2e/run.sh deps    # install qemu/dnsmasq/ipxe/edk2 + matchbox
./scripts/netboot-e2e/run.sh up      # bridge + dnsmasq + matchbox
./scripts/netboot-e2e/run.sh boot 52:54:00:00:00:11 worker1
./scripts/netboot-e2e/run.sh down    # tear everything down
```

## Status

Iteration 1: the lab plumbing (`deps`/`up`/`boot`/`down`). The full E2E
orchestration — bootstrap a 1-node CP, seed Medea + `capture-secrets` + register
the worker MAC + `enable-provisioning`, run the reconciler, boot the blank worker,
and assert join + `Host`→`Ready` + member — lands next, on top of this lab.

The PXE/boot specifics (arm64 UEFI netboot order, iPXE arch loaders, Talos asset
URLs) need real runs to settle; this is a multi-iteration build, slow on M1/M2.
