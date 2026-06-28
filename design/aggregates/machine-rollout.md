# Aggregate: MachineRollout (per-node execution)

**Context:** Version Rollout (core domain) · **Type:** aggregate (reconciler-owned) ·
**Status:** Implemented (OS path)

`MachineRollout` is the **per-node execution state machine** of an OS upgrade —
the running progress of one node moving to a target Talos version. It is the
mechanism a [`Rollout` job](rollout.md) drives, and where the load-bearing
safety properties (halt-on-failure, snapshot-before-control-plane,
PDB-respecting drain) actually live.

- **Type:** `pb.MachineRollout` ([`gen/medea/v1`](../../gen/medea/v1))
- **Owned/written by:** `rollout.Reconciler` (`setState`, `fail`) →
  `store.PutMachineRollout` ([`internal/rollout`](../../internal/rollout))
- **Driven by:** `rollout.Reconciler.ReconcilePool` → `upgradeNode` →
  `waitHealthy`
- **Authorized by:** the `Rollout` job (`rollout.Executor` builds the reconciler
  per job)

## Consistency boundary

- **One record per `cluster/addr`**, stored in `bRollouts/machines`
  (`store.PutMachineRollout`/`GetMachineRollout`) — distinct from the `Machine`
  identity record in `bDesired/machines` (`datastore.md` §3). The two never
  collide despite the shared sub-bucket name.
- **Reconciler-owned, last-writer-wins.** A single logical owner (the reconciler
  driving the pool) writes it, so there is no CAS — unlike desired records.
- **The boundary is one node.** The pool-level orchestration (which node next,
  `maxUnavailable` accounting) lives in `ReconcilePool` over the
  [`NodePool`](nodepool.md), not in this record. This record only tracks one
  node's transition.

## Lifecycle / state machine

`RolloutState`, driven by `upgradeNode` (`design/rollout-controller.md` §2.1, §5):

```
  Idle ──▶ Draining ──▶ Upgrading ──▶ WaitingHealthy ──▶ Done
            │             │               │
            └─────────────┴───────────────┴────────▶ Failed   (halts the rollout)
```

Per node, `upgradeNode` runs:

1. **version check** — if the node already reports `target`, set `Done` and skip
   (the basis of idempotent resume).
2. **budget check** — if other members already unavailable ≥ `maxUnavailable`,
   return and leave the node for a later pass (no state change).
3. **snapshot** (control-plane only, if `snapshotBeforeControlPlane`) — stream an
   etcd snapshot to a local file *before* any mutation; failure → `Failed`.
4. **`Draining`** — `kube.Drain` (cordons internally, PDB-respecting, **no
   force**); error/timeout → `Failed`.
5. **`Upgrading`** — read the node's current install image, derive the target
   image preserving the schematic (`talos.DeriveInstallerImage`), `UpgradeOS`
   (node reboots); error → `Failed`.
6. **`WaitingHealthy`** — `waitHealthy` polls until the node is `NodeReady` *and*
   reports `target`, up to `WaitTimeout`; connection errors during the reboot
   are treated as not-ready-yet (park), not failure; timeout → `Failed`.
7. **uncordon** → `Done`.

**Resume is re-derivation, not state-replay.** On boot the executor re-drives a
`Running` job and `ReconcilePool` re-evaluates each member by reading its *live*
Talos version (already-at-target → `Done`), which is what makes re-running
idempotent. The persisted `state` field is **progress/observability** — the
reconciler does not branch on it. (The narrative in `rollout-controller.md` §4
describes re-entering "at the recorded point"; the shipped code achieves the
same resume property via the version check instead. Code is authoritative.)

## Invariants

| # | Invariant | Enforced by | Why |
| --- | --- | --- | --- |
| I1 | **At most one node per pool is in flight at a time** (sequential processing). | `ReconcilePool` (returns as soon as a node can't complete this pass) | Guarantees ≤1 member unavailable, satisfying any `maxUnavailable ≥ 1` without parallel-disruption reasoning (`rollout-controller.md` §6). |
| I2 | **A node won't start upgrading if other members are already unavailable beyond budget.** | `ReconcilePool` (`unavailableOthers(...) >= maxUnavailable` → return) | Bounds simultaneous disruption to the operator's `maxUnavailable`. |
| I3 | **A control-plane node is never mutated without a fresh etcd snapshot first; snapshot failure aborts before touching the node.** | `upgradeNode` (snapshot gate, then `fail` on error) | On single-member etcd the snapshot is the only undo (`rollout-controller.md` §3, `rollout-safety.md` §4). |
| I4 | **Drain never forces and never deletes emptyDir by default; a drain timeout halts and surfaces the blocking pod.** | `kube.Drain` (no force) + `upgradeNode` (`fail` on drain error) | A stuck drain is usually a real availability constraint; forcing is the operator's explicit call, not the default. |
| I5 | **The upgrade preserves the node's schematic and bumps only the version.** | `talos.DeriveInstallerImage(curImage, target)` | A bare `installer:<version>` silently drops Image-Factory system extensions — a real regression on this netboot/Factory-provisioned cluster (`talos-client.md` §3). |
| I6 | **The first node that fails to drain/upgrade/become healthy halts the entire rollout.** | `fail` (sets `Failed`, returns an error) → `ReconcilePool` returns it → the job goes `Failed` | Prevents a bad image marching across the fleet — the core difference from a `for` loop of `talosctl upgrade` (`rollout-controller.md` §3). |
| I7 | **Cluster-unreachable mid-reboot is parked, not failed.** | `waitHealthy` (treats `NodeReady`/`Version` errors as not-ready-yet until `WaitTimeout`) | The control-plane apiserver legitimately disappears during its own reboot; that is expected, not an error (`rollout-controller.md` §4). |
| I8 | **Re-issuing an upgrade on an already-upgraded node is a no-op.** | `ReconcilePool` (version check precedes any mutation) | Makes restart/resume idempotent. |
| I9 | **Progression stops at a safe point when the pool is paused** (between nodes, never mid-node). | `ReconcilePool` (returns early if `NodePool.paused`); the in-flight node finishes its transition first | Pause/abort must never leave a node half-upgraded (`rollout-controller.md` §3, `rollout-safety.md` §3 #5). |

## Command surface

`MachineRollout` is **not** directly commanded — it is produced by the reconciler
under a job. The operator influences it indirectly:

- via the owning [`Rollout` job](rollout.md) (create / the executor),
- via `NodePool.paused` (`PauseRollout`/`ResumeRollout`) which gates I9,
- read-only via `GetRollout` (`medea rollout status [-w]`), which returns the
  pool's `MachineRollout`s.

Reconciler tunables (`Reconciler.PollInterval` ≈ 5s, `WaitTimeout` ≈ 10m) are set
small in tests; they are process config, not part of the aggregate.

## Event surface

- `machine_rollout` — emitted on every state transition (`setState`/`fail`),
  consumed by `Watch` for live status.

## Cross-context dependencies

- **Cluster Inventory** (reads): `NodePool` (`members`, `strategy`, `paused`,
  `role`), `Cluster`/`NodePool` desired for the target version (inheritance:
  pool `""` → cluster), `Machine.role` (snapshot gate). Maps node addresses to
  Kubernetes node names via `kube.ListNodes`.
- **Talos Integration (ACL):** `TalosOps` — `Version`, `InstallImage`,
  `UpgradeOS`, `EtcdSnapshot`.
- **Kubernetes Integration (ACL):** `KubeOps` — `ListNodes`, `Drain`,
  `Uncordon`, `NodeReady`.
- **Local filesystem:** etcd snapshots are written to `snapshotDir` and the path
  is logged (v1; `rollout-controller.md` §6, `talos-client.md` §5).
- **Persistence:** `store.PutMachineRollout` (LWW).

## Key decisions

- **Sequential, one-node-at-a-time** (I1); parallel pool rollouts deferred
  (`rollout-controller.md` §6).
- **Resume by re-derivation from live version** (I8), not by replaying the
  persisted `state` — simpler and robust to a node that finished its reboot while
  Medea was down.
- **Snapshot to a local file and log the location** for v1; the path stays
  generic so the deferred backup feature reuses it (`talos-client.md` §5).
- **Deterministic node order** (sorted by address) for reproducibility;
  drain-cost-aware ordering deferred (`rollout-controller.md` §6).
- **Known gap:** `maxUnavailable` is honored only in the conservative sense (I1
  makes effective concurrency 1); values > 1 don't yet increase parallelism.
