# New-cluster bootstrap — Medea-driven cluster creation (Phase B)

**Status:** B-M1 + B-M2 implemented (single-CP); B-M3 (QEMU validation) pending
**Date:** 2026-07-04

Scope: Medea creates a **new single-control-plane Talos cluster from bare metal** —
generating the PKI, rendering and serving the first control-plane machine config,
bootstrapping etcd, and capturing the resulting client credentials — so the
planned `home-cluster` rebuild is **Medea-driven** and `_out/` never returns. This
is **Phase B** of "Medea owns the PKI" and the deferred
`provisioning-plane.md` §9 flow ("new-cluster creation / first-control-plane
bootstrap", the chicken-and-egg no-cluster-yet case).

Builds directly on:
- **Phase A** ([`credentials.md`](credentials.md)) — the `CredentialStore` that
  durably holds the **generated** PKI + client configs. B is unsafe without it.
- **The v2 provisioning plane** ([`provisioning-plane.md`](provisioning-plane.md))
  — the `Provisioner` (Matchbox) seam, schematic `Resolver`, boot-asset/install-image
  derivation, and machine-config rendering. B renders a *control-plane* config and
  *generates* (not captures) secrets; everything else is reused.

Out of scope (deferred past B): **HA / multi-control-plane** (etcd growth, joining
additional CP members); worker joins (that's v2 scale-out, already built);
backup/restore (v3); auto-repair (v4). B is deliberately the *single-CP* core.

## 1. Decisions (this pass — 2026-07-04)

1. **Single control plane only.** One CP node, `allowSchedulingOnControlPlanes`
   (the homelab runs workloads on it). HA (VIP, multi-CP etcd) is future work
   (§8) — the genuinely hard part of §9, and not what the rebuild needs.
2. **Endpoint is pinned up front** (resolves the chicken-and-egg). The create spec
   declares the control-plane endpoint (`https://<CP-IP>:6443`) and the CP host's
   MAC + reserved IP. The config's `controlPlaneEndpoint` and the address
   `talosctl` connects to are both that pinned IP (a DHCP reservation, as today).
   Single-CP → endpoint == node IP; no VIP.
3. **Medea generates the secrets bundle** (`secrets.NewBundle`) and owns it from
   birth — the inverse of v2 capture. Stored via the Phase A `CredentialStore`.
   Rejected: generating on the node / a human `talosctl gen` (defeats the point).
4. **Bootstrap is a guarded, resumable phase**, not a fire-and-forget command. A
   `ClusterBootstrap` phase (sibling of `ClusterRollout`) is advanced by a
   bootstrap reconciler; steps span a node reboot, so it must resume after a
   Medea restart (like `MachineRollout`). Gated default-off + plan/confirm
   (mirrors `rollout-safety.md`). Rejected: a one-shot imperative command (simpler
   but not resumable/observable, and creation crosses a reboot).
5. **Reuse the provisioning primitives**, add only what's genuinely new: CP config
   rendering, secrets *generation*, and two Talos ACL ops (`Bootstrap`,
   `Kubeconfig`). The Matchbox `Provisioner`, schematic `Resolver`, and
   `BootAssets`/`InstallImage` are used unchanged.
6. **Power-agnostic** (same stance as provisioning-plane §7): Medea stages the
   boot and waits; power-on is manual or WoL. No BMC.
7. **Per-cluster machine-config patches are inputs to the render** — the
   `home-cluster` `talos/` layer (`allowSchedulingOnControlPlanes`, CNI-none +
   the inline-Cilium manifest, install disk). This is the seam where the
   machine-config story (the config-rollout feature) meets bootstrap.

## 2. The bootstrap flow (a resumable phase machine)

A `ClusterBootstrap` phase, reconciler-owned (LWW), persisted so it survives the
CP node's install-reboot and a Medea restart:

```
NotBootstrapped
  → GeneratingSecrets   secrets.NewBundle → CredentialStore.PutSecrets(cluster)
  → Staging             render CP config (spec + secrets + patches + schematic);
                        Provisioner.Stage(mac, profile, config);
                        derive + store talosconfig now (endpoint is known)
  → AwaitingInstall     node PXE-boots, installs to disk, reboots.
                        Talos API unreachable is EXPECTED → park-and-retry
                        (errors.Is(err, ErrUnreachable)), not a failure.
  → Bootstrapping       Talos API reachable → talos.Bootstrap(node) EXACTLY ONCE
  → AwaitingHealthy     etcd member up + kube-apiserver responding
  → FetchingKubeconfig  talos.Kubeconfig(node) → CredentialStore.Put(kubeconfig)
  → Ready               seed inventory: Cluster/NodePool/CP Machine desired =
                        reality; the CP Host → Ready
  → Failed              any step errors or overruns its timeout → halt for the
                        operator (no BMC ⇒ no remote console); re-runnable
```

- **Park-and-retry** on `AwaitingInstall`/`AwaitingHealthy` reuses the rollout
  discipline: a booting/rebooting node is unreachable-expected, the reconciler
  backs off and retries rather than failing (`talos-client.md` §1).
- **Halt-on-failure**: a timed-out install/bootstrap stops the flow for operator
  attention (a half-installed disk needs a wipe before retry — §9), mirroring the
  rollout's halt.

## 3. What's new vs reused

| Piece | Status |
| --- | --- |
| Matchbox `Provisioner.Stage`, schematic `Resolver`, `BootAssets`/`InstallImage` | **Reused** (provisioning-plane, built) |
| `CredentialStore` (holds generated secrets + talos/kubeconfig) | **Reused** (Phase A, credentials.md) |
| Secrets **generation** (`secrets.NewBundle`) | **New** — `provision.GenerateSecrets` (local; parallels `CaptureSecrets` but mints) |
| `RenderControlPlaneConfig` (`input.Config(machine.TypeControlPlane)` + patches + `allowSchedulingOnControlPlanes`) | **New** — sibling of `RenderWorkerConfig` in `provision/config.go` |
| `talos.Bootstrap(ctx, node)` — one-time etcd init | **New** — Talos ACL op (machinery bootstrap) |
| `talos.Kubeconfig(ctx, node)` — fetch admin kubeconfig | **New** — Talos ACL op |
| `ClusterBootstrap` phase + bootstrap reconciler + guard + `medea cluster create` | **New** — Provisioning-context reconciler (sibling of the join reconciler) |

## 4. Domain placement (the domain-model checklist)

- **Context:** primarily **Provisioning** (v2) — a new reconciler beside the
  join-existing one. It reaches into **Security & Credentials** (generated PKI →
  `CredentialStore`), the **Talos ACL** (`Bootstrap`/`Kubeconfig`), and, at the
  end, **Cluster Inventory** (seeds `Cluster`/`NodePool`/`Machine` desired).
  Spanning contexts is expected here — creation is the act that *originates* the
  inventory; the write ordering keeps each within its owner.
- **Aggregate / consistency boundary:** the `ClusterBootstrap` phase is a
  reconciler-owned record (LWW, like `ClusterRollout`), keyed by cluster. The CP
  `Host` is the provisioning target. One-record writes only; the final "seed
  inventory" is a sequence of single-record desired writes (Inventory-owned), not
  a transaction.
- **Ubiquitous language:** reuse *Host*, *secrets bundle*, *endpoint vs node*,
  *park-and-retry*, *plan/confirm*, *halt-on-failure*. New term: **bootstrap**
  (the one-time etcd init + cluster origination) — add to the `DOMAIN.md` glossary
  when code lands. Do **not** overload "rollout".
- **Invariants (what holds → enforced by → why):**
  - *Bootstrap runs exactly once* → only the `Bootstrapping` transition calls
    `talos.Bootstrap`, and the phase is CAS-persisted so a restart resumes *past*
    it → running it twice re-inits and destroys etcd.
  - *Secrets are generated once per cluster* → `GeneratingSecrets` is skipped if
    the `CredentialStore` already has a bundle for the cluster → re-generating
    would mint a *different* cluster the staged config doesn't match.
  - *Endpoint is fixed before `Staging`* → the create spec requires it; render
    fails without it → the config and `talosctl` target must agree before boot.
  - *Creation is deliberate* → default-off guard + plan/confirm, stronger confirm
    than an upgrade (it originates a cluster) → mirrors `rollout-safety.md`.
- **Events:** a new `cluster_bootstrap` phase event (parallel to
  `cluster_rollout`) so `medea cluster create -w` and the CLI can watch progress;
  the terminal seed emits the usual `cluster`/`nodepool`/`machine` events.
- **Posture:** `Bootstrap`/`Kubeconfig` live behind the Talos ACL (no upstream
  types leak); secrets generation is a local op; no behavior on proto types;
  timestamps stamped at the reconciler edge (phase-started, for timeouts).

## 5. Machine-config rendering (control plane)

`RenderControlPlaneConfig` mirrors `RenderWorkerConfig` (provisioning-plane §5)
with three differences:

1. `input.Config(machine.TypeControlPlane)` (not worker).
2. **Generated** secrets bundle (`GenerateSecrets`), stored to the `CredentialStore`
   *before* render — Medea is the PKI owner from t=0.
3. **Per-cluster patches** applied on top: `allowSchedulingOnControlPlanes` (single
   node runs workloads), `cluster.network.cni.name: none` + the inline-Cilium
   manifest, kube-proxy disabled, the install disk — i.e. the `home-cluster`
   `talos/` layer, supplied in the create spec. This is the concrete tie-in to the
   machine-config-rollout feature: the same desired-config that a config rollout
   would later reconcile is what bootstrap first *applies*.

The rendered config (secret-bearing) is written only to Matchbox for the node to
fetch over the LAN — never to bbolt, never to `Export` (unchanged invariant).

## 6. Client credentials

- **talosconfig** is derivable from the generated bundle + the pinned endpoint at
  `Staging` time (no cluster needed) → stored immediately so `talosctl`/the
  reconciler can reach the node as it comes up.
- **kubeconfig** only exists after bootstrap → `talos.Kubeconfig(node)` fetches the
  admin kubeconfig once `AwaitingHealthy` clears → stored via the `CredentialStore`.

Both land in the Phase A store (file or 1Password). Combined with `GetCredentials`
(A-M2), an operator gets working `kubectl`/`talosctl` for a freshly-created cluster
with no `_out/`.

## 7. `medea cluster create`

```
medea cluster create --name home \
  --cp-endpoint https://192.168.14.160:6443 \
  --cp-mac <mac> --cp-ip 192.168.14.160 \
  --talos-version v1.13.5 --kubernetes-version v1.36.1 \
  --extensions siderolabs/iscsi-tools,siderolabs/util-linux-tools \
  --patch @talos/patches/controlplane.yaml --patch @talos/patches/cilium-inline.yaml
      # PLAN: shows the endpoint, schematic, patches, and steps; creates nothing.
medea cluster create ... --confirm
      # creates the Cluster record in NotBootstrapped and arms the phase.
```

The bootstrap reconciler (under `serve --provisioning`, gated by the per-cluster
guard) advances the phase §2. `--confirm` on create is the deliberate act;
progress is watchable (`-w`).

## 8. Future work (past Phase B)

- **HA / multi-control-plane** — a VIP endpoint, generating + joining additional
  CP members, etcd growth. The endpoint decision (§1 #2) is where this plugs in.
- **Worker join at create** — B stands up the CP; workers come from the existing
  v2 scale-out reconciler once the cluster exists (they compose naturally).
- **Wipe-before-retry** — a failed/half-installed node needs its disk wiped before
  re-provisioning (ties to the deprovision path + `Power` for a real off→on).
- **Bootstrap from a restore** — v3's `RestoreEtcd` onto a freshly bootstrapped CP
  is the v4 control-plane-repair path; it reuses this flow + `backup.md`.

## 9. Open questions

- **Phase location** — RESOLVED (B-M2): a separate `ClusterBootstrap` record
  (like `ClusterRollout`), for a clean event + resume story.
- **Patch supply** — patches inline in the create spec vs a reference Medea reads.
  Inline is simplest for B; a referenced desired-config is the config-rollout
  feature's job.
- **Idempotent re-run** of a partially-bootstrapped cluster — resume from the
  persisted phase (preferred) vs require an explicit wipe/reset first.
- **Disk-not-empty** — a re-used node may boot the old install instead of PXE;
  the create flow may need to assert/force a wipe (Talos `wipe` / maintenance).
- **Endpoint health probe** — exact "etcd up + apiserver responding" signal for
  `AwaitingHealthy` (reuse the rollout's health check vs a dedicated probe).

## 10. Test strategy (maps to PRD §9)

- **Unit (fakes):** `RenderControlPlaneConfig` (golden config: role, patches,
  install image, `allowSchedulingOnControlPlanes`); the bootstrap phase machine
  (transitions, bootstrap-once guard, secrets-generated-once, park-and-retry on
  unreachable, timeout → Failed) against fake `Provisioner`/`Talos`/`kube`/store.
- **Integration:** real `talos.Bootstrap`/`Kubeconfig` against a QEMU CP node;
  real schematic resolve + Matchbox stage (reused from provisioning-plane).
- **E2E (QEMU, pre-release):** create a cluster from an empty VM — generate →
  stage → boot → bootstrap → healthy → kubeconfig — assert `kubectl get nodes`
  works and the CP is Ready; then a real Beelink CP. Reuses the `make test-qemu`
  harness. Never rehearsed first on anything precious.
