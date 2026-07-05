# Provisioning plane (v2)

**Status:** Draft for review
**Date:** 2026-06-28

Scope: how Medea turns bare metal into cluster members — the Layer-0 it has
deferred since v1 (PRD §4, §6). v2 covers **adding nodes to an existing,
already-seeded cluster** (worker scale-out and node replacement); it does **not**
create new clusters (first-control-plane bootstrap is deferred — §9). It absorbs
the hand-run `netboot/` setup in `talos-cluster` (Matchbox + dnsmasq/TFTP),
per convergence decision PRD §13 #11. Blocks the v2 milestones (§8). Auto-repair
(v4) and backup/restore (v3) build on this; see PRD §14.

Out of scope here: the rollout reconciler (v1, `rollout-controller.md`); backup
scheduling/restore (`design/backup.md`, planned); auto-repair and power control
(v4 — only the seam is noted, §7).

## 1. Decisions (this pass — 2026-06-28)

Resolved in the v2 design conversation:

1. **Sequencing: Provisioning (v2) → Backup/Restore (v3) → Auto-repair (v4).**
   Dependency order; each independently shippable. Restore is also what
   control-plane auto-repair needs, so it precedes repair.
2. **Boot mechanism: Medea drives Matchbox.** Medea owns Matchbox
   profiles/groups and the rendered machine configs, and orchestrates the
   existing dnsmasq proxy-DHCP + TFTP iPXE chainload. Reuses the `netboot/`
   investment rather than reimplementing a boot server (§3). Rejected:
   Medea-as-boot-server (reimplements working infra); Sidero Metal (CRD /
   management-cluster shaped — reintroduces the in-cluster dependency v1
   rejected, PRD App. B).
3. **Inventory + membership: a MAC-keyed `Host` aggregate; `NodePool` gains
   `replicas` + a hardware `selector`; membership becomes reconciler-managed**
   (the CAPI/Metal3/Sidero shape). Enables scale and repair (§2, §4).
4. **v2 scope = add nodes to an existing cluster.** Reuses the cluster's
   existing secrets; no first-CP bootstrap (§9).
5. **Machine-config generation: Medea generates from a spec.** Medea renders the
   base config from a `Cluster`/`NodePool` spec + per-node patches and writes it
   into Matchbox; it is the source of truth for node config (§5).
6. **Schematic: Medea resolves via the Image Factory API.** A pool declares an
   extension set; Medea creates/pins the schematic ID and uses it for boot
   assets + install image (§6).
7. **Secrets: Medea owns the cluster machine-secrets bundle.** Captured from the
   live cluster into the `CredentialStore`, used to mint join configs; never in
   bbolt, never exported (§5, `api-and-auth.md` §5).
8. **Provisioning is power-agnostic.** It stages the boot and waits for the node
   to come up; power-on is manual or Wake-on-LAN. A `Power` interface is a v4
   seam, unimplemented in v2 (§7).

## 2. The `Host` aggregate (new) and `NodePool` changes

A **`Host`** is a piece of bare metal Medea knows about *before* it is a cluster
member — the bridge the v1 model lacked. New proto message in the shared kernel
(`gen/medea/v1`), a new Cluster-Inventory-context aggregate (`DOMAIN.md` will
gain it when code lands).

```
Host:
  mac: string            # identity (Matchbox groups key on MAC); the one stable id
  cluster: string        # owning cluster (once allocated)
  pool: string           # owning pool (once allocated)
  labels: map<str,str>   # for NodePool.selector matching (e.g. role, arch, disk)
  addr: string           # observed once it boots + joins (the Talos endpoint)
  state: HostState
  message: string
  revision: uint64
```

`HostState` lifecycle (reconciler-owned, like the rollout records):

```
  Registered ──▶ Allocated ──▶ Provisioning ──▶ Ready
                                    │
                                    └────▶ Failed   (provision timed out / errored; halts)
       ▲                                              │
       └──────────────── Deprovisioning ◀────────────┘ (release / replace)
```

- **Registered** — operator added the MAC; not yet assigned (Available pool).
- **Allocated** — the provisioning reconciler bound it to a pool to satisfy
  `replicas` (or a replacement).
- **Provisioning** — Matchbox profile/group + machine config written; waiting for
  the node to PXE-boot, install, and join.
- **Ready** — node joined, Talos-healthy, `addr` observed; a `Machine` is bound.
- **Failed** — did not converge within the timeout (halt; operator
  investigates — no BMC means no remote console, §7).
- **Deprovisioning** — releasing a Host (node replacement / scale-in): wipe on
  next boot, remove from Matchbox, return to Registered/removed.

**`NodePool` additions:**

```
NodePool (additions):
  replicas: uint32          # desired member count; reconciler converges to it
  selector: map<str,str>    # match against Host.labels
  # members stays, but becomes reconciler-managed (was an explicit list in v1)
```

v1 pools keep working: `replicas == 0` (unset) + a non-empty `members` list = the
existing "manage these exact addresses" behavior. A pool opts into provisioning
by setting `replicas` + `selector`.

## 3. Matchbox driver

Medea owns the Matchbox state the operator edits by hand today
(`netboot/matchbox/groups/*.json`, profiles, `scripts/sync-configs.sh`).

- **Interface (ACL, like talos/kube):** a small `Provisioner` seam —
  `Stage(host, profile, machineConfig)` / `Unstage(host)` — so the reconciler is
  testable with a fake and the Matchbox specifics live in one package
  (`internal/provision/matchbox`).
- **Write path — open question (§10):** Matchbox exposes a gRPC API *and* a
  file-backed store. The current setup is file-based (sync script). Leaning
  toward the **gRPC API** (no shared filesystem assumption, cleaner for a
  remote Matchbox), with a file-backed impl as the fallback that matches today.
- **What gets written per Host:** a *group* keyed by MAC → a *profile*
  (kernel/initrd for the resolved schematic, kernel cmdline pointing at the
  machine-config endpoint) → the rendered machine config.
- **dnsmasq/TFTP** (proxy-DHCP + iPXE chainload, needed because the Beelink UEFI
  HTTP-boot is broken) stays as-is operationally; Medea does not manage dnsmasq
  in v2 (it is static infra). Documented as a deployment dependency.

## 4. Provisioning reconciler (join-existing flow)

A new reconciler in the Version-Rollout-sibling style (a controller over
desired-vs-observed in the store). Per pool with `replicas` set:

```
desired = pool.replicas ; have = count(Ready hosts in pool)
if have < desired:
    pick an Available Host matching pool.selector  (else: wait — no capacity)
    Host: Registered -> Allocated
    render machine config (§5) + resolve schematic (§6)
    Provisioner.Stage(host, profile, config) ; Host -> Provisioning
    wait until: node appears in kube AND Talos-healthy AND version matches
        on timeout -> Host=Failed (halt; surface for the operator)
    bind a Machine to the Host ; add addr to pool.members ; Host -> Ready
if have > desired:   (scale-in / replacement)
    pick a victim ; cordon+drain (reuse rollout's kube ops) ; Deprovisioning
    Provisioner.Unstage ; remove Machine + member ; Host -> Registered/removed
```

- **Reuses v1 primitives:** the `kube` drain/cordon ops (scale-in), the
  observed-state refresh (to detect "node joined"), and the same halt-on-failure
  + park-and-retry discipline (a booting node is unreachable — expected, not a
  failure, exactly like the rollout wait).
- **Safety:** gated behind the same per-cluster `rolloutsEnabled`-style guard
  (a `provisioningEnabled`, default off) and the global executor flag, so
  provisioning can never act on a cluster by accident (mirrors
  `rollout-safety.md`). Plan/`--confirm` for destructive scale-in.
- **One provisioning op per pool at a time** in v2 (like the rollout's sequential
  default); parallel provisioning is future work.

## 5. Machine-config generation + secrets

Medea renders a joining node's Talos machine config from three inputs:

1. **Base** — derived from the `Cluster`/`NodePool` spec (role worker/CP, install
   disk, kubelet args, registries, CNI expectations). v2 targets worker joins
   (CP join needs HA, out of scope — §9).
2. **Cluster machine-secrets bundle** — the CA, machine token, cluster id/secret,
   bootstrap token, and control-plane endpoint a joining node needs. **Captured
   from the live cluster**, not regenerated: Medea reads an existing node's
   `MachineConfig` via the Talos API (COSI resource) and extracts the bundle into
   the `CredentialStore` (`<cluster>/secrets.yaml`). This is a one-time capture
   per cluster (re-runnable). Rejected: `talosctl gen secrets` (mints *new*
   secrets — wrong for joining an existing cluster).
3. **Per-node patches** — hostname, install disk, static IP / DHCP reservation,
   labels.

Secrets never enter bbolt and are never in `Export` (`api-and-auth.md` §5,
`datastore.md` §9). The rendered config (which contains secrets) is written only
to Matchbox for the node to fetch at boot, over the LAN.

## 6. Schematic resolution (Image Factory)

A pool declares its extension set (`PoolSpec.extensions`, empty = stock). Medea
calls the **Image Factory API** to create/resolve the schematic, pins the
returned **schematic ID**, and uses it to fetch boot assets (kernel/initrd for
PXE) and to derive the install image — the same `factory.talos.dev/...`/`:version`
shape the v1 rollout already preserves (`talos-client.md` §3,
`talos.DeriveInstallerImage`). Pinning the ID means a node re-provisions to the
same extension set deterministically. Stock no-extensions (today's case) is just
the empty-extension schematic.

The factory boot-asset URLs are **HTTPS**, but iPXE (in the lab and on the
Beelinks) has no TLS and silently fails on `kernel https://...`. So the Matchbox
driver **mirrors** the kernel/initrd it is given — fetching them from the factory
over HTTPS (which Medea can) into its `assets/` dir and rewriting the profile to
serve them over plain **HTTP** (`matchbox.Store.Stage`). `BootAssets` still returns
the factory source URLs; localization is the driver's job. This supersedes the
hand-run `netboot/` asset fetch.

## 7. Power control (v4 seam, not built in v2)

Provisioning stages the boot and waits; it does not power the node on. A
**`Power` interface** (`On`/`Off`/`Cycle(host)`) is reserved as the seam
auto-repair (v4) will implement — candidate impls: Wake-on-LAN, a smart-plug/PDU
API, or Redfish where a BMC exists. The Beelinks have no BMC (no management NIC),
so the realistic primitives are **WoL** (powers a cleanly-off node on; cannot
recover a *hung* node) and a **smart plug** (true off→on cycle — the prerequisite
for hands-off repair of a frozen node). v2 does not depend on any of these;
power-on is manual or WoL. Capability check is an operator task (BIOS WoL +
magic-packet test; nmap 623/Redfish probe for BMC).

## 8. Milestones (v2)

- **v2-M1 — Inventory + spec.** `Host` proto + store (CAS/LWW like v1 records);
  `NodePool.replicas`/`selector`; `medea host register|list`; secrets capture
  into `CredentialStore`. Read-only — no booting yet.
- **v2-M2 — Matchbox driver + config gen.** `Provisioner` seam + Matchbox impl;
  machine-config rendering; schematic resolution. Unit-tested with fakes.
- **v2-M3 — Provisioning reconciler.** Join-existing flow (scale-out), gated by
  `provisioningEnabled` + plan/confirm; integration-validate a real worker join
  on the QEMU/docker tier; then a real Beelink worker.
- **v2-M4 — Scale-in / replacement + hardening.** Deprovision/drain flow; docs;
  CI tier for provisioning.

## 9. Future work (deferred past v2)

- **New-cluster creation** — first-control-plane bootstrap (gen secrets → apply
  → `talosctl bootstrap` etcd). The chicken-and-egg no-cluster-yet flow. **Now
  designed** (single-CP) in [`cluster-bootstrap.md`](cluster-bootstrap.md) (Phase B).
- **Control-plane node provisioning / HA** — joining additional CP members
  (etcd growth); ties into the non-HA → HA story.
- **Auto-repair (v4)** — failure detection + the `Power` impl; reprovision a dead
  node. Restore (v3) underpins CP repair.
- **DHCP-discovery of unknown hardware** (v2 = manual MAC registration).
- **Parallel provisioning** across hosts/pools.

## 10. Open questions

- **Matchbox write path** — RESOLVED (v2-M2): **file-backed**, validated against
  a real Matchbox v0.11 in docker (`TestMatchboxServesStagedHost`). Two contract
  details the integration test pinned: the profile field is **`generic_id`** (not
  `generic_config`), and the driver must add the **`talos.config=<matchbox>/generic?mac=…`**
  kernel arg or a booted node never fetches its config. A gRPC-API impl stays an
  option behind the same seam.
- **Host discovery** — v2 is manual `host register --mac`. Is DHCP-snoop
  discovery worth it for a 3-node homelab? Likely not yet.
- **IP assignment** — static patch vs DHCP reservation (dnsmasq already hands out
  leases). Lean DHCP reservation keyed by MAC, matching today.
- **Install-disk selection** — fixed device path vs a selector (Beelinks are
  uniform; a fixed path is fine for v2).
- **Replacement identity** — when replacing a dead node, does the new Host reuse
  the old node's hostname/IP, or get a fresh identity? (Affects rollout/observed
  bookkeeping.)

## 11. Prior art

| System | Shape | Relation |
| --- | --- | --- |
| **Sidero Metal** | Talos-native metal provisioner; `Server`/`ServerClass` CRDs, PXE/iPXE, matchbox-like. | Closest fit; but CRD/management-cluster shaped — the in-cluster coupling Medea rejects (PRD App. B). We take its *shape* (host class + selector), not its placement. |
| **Metal3 / BareMetalHost** | The canonical bare-metal inventory CRD; Ironic + BMC (IPMI/Redfish) for power/provision. | Source of the `Host` (MAC/BMC) model. We drop the BMC assumption (Beelinks have none) — hence the power-agnostic stance. |
| **Cluster API (CABPT/Metal3)** | Declarative infra/bootstrap providers in a mgmt cluster. | Same rejection as v1 (Appendix B). |
| **Tinkerbell** | Workflow-engine bare-metal provisioning (DHCP/iPXE/templates). | Alternative boot stack; heavier than Matchbox for a 3-node lab. |
| **Matchbox** | iPXE/Ignition/config matcher by MAC. | What `netboot/` already runs; Medea drives it (decision #2). |

## 12. Test strategy (maps to PRD §9)

- **Unit (fakes):** the provisioning reconciler (replicas convergence, selector
  matching, halt-on-failure, scale-in drain) against a fake `Provisioner`/`kube`;
  config rendering (spec + secrets + patches → expected machineconfig); schematic
  resolution (fake Factory).
- **Integration:** render a real config and stage it against a real Matchbox;
  resolve a real schematic against the Factory API.
- **E2E (pre-release):** a real worker join on the QEMU tier (boot an empty VM →
  provision → joins the cluster), then a real Beelink worker. Reuses the
  `make test-qemu` harness shape.
