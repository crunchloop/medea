# Aggregate: Machine

**Context:** Cluster Inventory (supporting) · **Type:** aggregate ·
**Status:** Implemented

`Machine` is one node. It carries the node's **identity** (its Talos endpoint
address), its `role` and `pool` membership, and its **observed** phase/versions/
health. Identity is operator/seed-set; observed is reconciler-projected. It is
the finest-grained inventory record.

- **Type:** `pb.Machine` ([`gen/medea/v1`](../../gen/medea/v1))
- **Identity written by:** `seed.Apply` ([`internal/seed`](../../internal/seed))
- **Observed projected by:** `refresh.Refresher` ([`internal/refresh`](../../internal/refresh))
- **Read by the core domain:** `rollout.Reconciler` (for `role`)

## Consistency boundary

- **One record per `cluster/addr`**, stored in `bDesired/machines`
  (`store.PutMachineDesired`/`GetMachine`, key = `cluster\x00talosEndpoint`).
- **Identity is persisted via CAS; observed is in-memory only**
  (`store.SetMachineObserved`) and never written to bbolt (`datastore.md` §2).
- The per-node *rollout progress* is a **separate** aggregate
  ([`MachineRollout`](machine-rollout.md)) in `bRollouts/machines` — same key,
  different top-level bucket, no collision.

## Lifecycle

- **identity** — created by `seed` (address, role, pool) and otherwise stable in
  v1 (existing nodes; provisioning would later create/destroy machines).
- **observed** — overwritten each refresh pass: `phase`, `talosVersion`,
  `kubernetesVersion`, `healthy`.

`MachinePhase` defines a richer set (`Provisioning`, `Booting`, `Joining`,
`Ready`, `Draining`, `Resetting`, `NotReady`), but **v1 refresh only projects
`Ready` / `NotReady`** — the others await the provisioning/repair pillars. Treat
the extra phases as a forward seam, not live state.

## Invariants

| # | Invariant | Enforced by | Why |
| --- | --- | --- | --- |
| I1 | **Identity is the Talos endpoint address**, used as the key within a cluster. | `store` key (`cluster\x00talosEndpoint`); `seed` requires a non-empty internal IP | A node needs a stable identity for targeting and progress tracking; the address is it in v1 (`talos-client.md` §2). |
| I2 | **Observed is a rebuildable in-memory cache, never persisted.** | `store.SetMachineObserved` (in-memory map) | Health/versions are always re-readable from the live cluster; persisting them adds nothing and risks stale truth (`datastore.md` §2). |
| I3 | **A node's `role` mirrors its pool's role.** | `seed` (`pbRole`/`poolName` set both consistently) | The reconciler keys the snapshot gate off `Machine.role`; it must agree with the pool. |
| I4 | **An unreachable node mid-reboot leaves `talosVersion` blank and `healthy=false` rather than failing the pass.** | `refresh.refreshCluster` (bounded per-node version read; missing node → `NOT_READY`) | Refresh must degrade gracefully while a node is rebooting (cross-ref [`MachineRollout`](machine-rollout.md) I7). |

## Command surface

- `ListMachines` (`medea get machines --cluster … [--pool …]`) — read only.
- **No direct operator mutation.** Identity comes from `seed`; observed from
  `refresh`. The operator acts on the [`NodePool`](nodepool.md), not individual
  machines.

## Event surface

- `machine` — emitted when an identity record is written (seeding). Observed
  changes do **not** emit events.

## Cross-context dependencies

- **Version Rollout** reads `role` (snapshot gate) and uses the address as the
  upgrade target; node-name resolution for drain comes from `kube.ListNodes`.
- **Kubernetes / Talos Integration:** `refresh` populates observed via
  `kube.ListNodes` (readiness, kubelet version) + `talos.Version`.
- **Persistence + Shared Kernel:** `store.Store`; `pb.Machine` is kernel.

## Key decisions

- **Address-as-identity in v1** (I1); a provisioning-era model may introduce
  hardware identity and reconciler-managed membership (PRD §7.2).
- **Observed split mirrors the cluster's** (I2) — the same CQRS read-model
  projection, at node granularity.
- **Known gap:** `MachinePhase` is largely aspirational in v1 — only
  `Ready`/`NotReady` are ever projected; the provisioning/repair phases land with
  those pillars.
