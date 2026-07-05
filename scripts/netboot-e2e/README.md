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

## Bootstrap rehearsal (Phase B) — `bootstrap-rehearsal.sh`

The other E2E on this lab: rehearse the **Medea-driven cluster-create** flow
end-to-end on a throwaway VM before touching the Beelinks
(`design/cluster-bootstrap.md` §10, B-M3). This is the CREATE path (a fresh CP +
new PKI), distinct from the worker-join E2E sketched under Status below.

Flow (`bootstrap-rehearsal.sh all`, built on `lab.sh`):

```
lab up  →  medea serve --bootstrap --provisioning (matchbox = the lab's)
        →  medea cluster create dap --cni none --disable-kube-proxy --confirm
        →  CP VM PXE-boots the Medea-staged profile → Talos installs + reboots
        →  medea: bootstrap etcd → apiserver healthy → Ready (seeds inventory)
        →  make bootstrap-cni (helm install Cilium, from home-cluster)
        →  assert node Ready + cilium DaemonSet up  →  teardown
```

Individual steps are also subcommands (`up|medea|create|boot|wait|cni|assert|down`)
for iterating without re-running the whole chain.

**Reused vs new.** Reused: the bridge + dnsmasq + matchbox + PXE `boot` from
`lab.sh`, and its asset-mirroring trick (cache from Image Factory over HTTPS
host-side, serve from matchbox over plain HTTP). New: driving `medea serve` +
`cluster create` at the lab's matchbox, the `make bootstrap-cni` step (the B1 CNI
install — Medea stays CNI-agnostic), and the terminal assertions.

**Three things need real runs to settle** (flagged inline in the script):

1. **iPXE can't HTTPS (blocker).** Medea's `BootAssets` returns `factory.talos.dev`
   HTTPS URLs; the lab's iPXE (and the Beelinks) can't fetch them, so the staged
   profile's kernel/initrd must be **mirrored through matchbox over HTTP** — exactly
   what `lab.sh stage-stock` does by hand. Until Medea does this
   (SESSION-HANDOFF §2a), the CP parks at `AwaitingInstall`. This rehearsal is what
   validates that fix.
2. **CP fixed IP.** `--cp-ip` must equal the address the CP actually gets. Add a
   dnsmasq reservation (`dhcp-host=<cp-mac>,<cp-ip>`) — `lab.sh` doesn't pin one yet.
3. **Boot order.** After install the node must boot the **disk**, not PXE again —
   use disk-first + network-fallback (the same order recommended for the Beelink
   BIOS). `lab.sh boot` currently forces `-boot n`.

Plus: the 30-minute bootstrap timeout is a reconciler const with no serve flag —
under TCG (M1/M2, no nested KVM) install alone can approach it, so run on a KVM
host/CI or make the timeout a flag first.

> **STATUS:** `bootstrap-rehearsal.sh` is a **skeleton** — the command sequence is
> real; the phase-poll (`wait`) and the three items above are stubbed/TODO. This is
> the multi-iteration build the note below anticipated.

## Status

Iteration 1: the lab plumbing (`deps`/`up`/`boot`/`down`). Two E2E orchestrations
sit on top: the **bootstrap rehearsal** above (Phase B, CREATE), and the original
**worker-join** flow — seed Medea + `capture-secrets` + register the worker MAC +
`enable-provisioning`, run the reconciler, boot the blank worker, and assert join +
`Host`→`Ready` + member.

The PXE/boot specifics (arm64 UEFI netboot order, iPXE arch loaders, Talos asset
URLs) need real runs to settle; this is a multi-iteration build, slow on M1/M2.
