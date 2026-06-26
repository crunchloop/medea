# Rollout controller

**Status:** Draft for review
**Date:** 2026-06-25

Scope: the v1 version-rollout reconciler — how Medea takes a change to a
cluster's or pool's desired Talos/Kubernetes version and converges the
fleet to it safely. Covers the per-node state machine, the two upgrade
mechanisms, the safety rails (`maxUnavailable`, drain, halt-on-failure,
snapshot-before-control-plane), and resume-after-reboot. Blocks PRD
milestones **M2** (OS path) and **M3** (K8s path + CP safety).

This doc assumes the datastore and Talos/kube clients exist (M1); it does
not specify those — see `datastore.md` and `talos-client.md`.

## 1. Trigger and inputs

> **Superseded by [`rollout-safety.md`](rollout-safety.md) (2026-06-26).** The
> drift-reconcile trigger in this section was reversed for safety: in v1
> (`mode: manual`) editing `desired` is **inert**; the reconciler runs only from
> an explicit, confirmed `Rollout` job on a `rolloutsEnabled` cluster. The state
> machine in §2 onward (drain → snapshot → upgrade → wait → uncordon,
> halt-on-failure) is unchanged — only *what invokes it* changed. Read the rest
> of this section as historical.

The reconciler watches the store for a delta between **desired** and
**observed** version on a `Cluster` or `NodePool`:

- `NodePool.desired.talosVersion` (or inherited `Cluster.desired.talosVersion`) ≠ observed across the pool's members → **OS rollout** for that pool.
- `Cluster.desired.kubernetesVersion` ≠ `Cluster.observed.kubernetesVersion` → **K8s rollout** for the whole cluster.

A rollout is never "started" by the CLI. The CLI only writes the desired
field; the reconciler is the sole driver. This keeps the store the single
source of truth (PRD §13 #2) and makes every action idempotent and
resumable.

## 2. Two mechanisms, two paths

Talos upgrades OS and Kubernetes differently (PRD §8.3). The reconciler
branches accordingly.

### 2.1 OS path — `talosctl upgrade` (Medea-driven, per-node)

Atomic A/B image swap + reboot, one node at a time. Medea owns the loop.

Per-pool algorithm:

```
nodes = pool.members sorted deterministically (workers before any CP; stable by address)
for each node where observed.talosVersion != desired:
    if (count of pool nodes currently unavailable) >= strategy.maxUnavailable: wait
    if node.role == controlplane and strategy.snapshotBeforeControlPlane:
        take etcd snapshot (block until stored) ; abort rollout if snapshot fails
    transition node.rollout.state: Idle -> Draining
        cordon(node)
        drain(node, timeout=strategy.drainTimeout, respectPDBs=true)
        on drain timeout -> HALT (state=Failed, surface blocking pod) ; do NOT force
    transition -> Upgrading
        talos.upgrade(node, image=for(desired.talosVersion))   # node reboots
    transition -> WaitingHealthy
        wait until: node Ready (kube) AND talos health OK AND observed.talosVersion == desired
        on timeout / unhealthy -> HALT (state=Failed)
    uncordon(node)
    transition -> Done
when all nodes Done -> pool rollout phase = Idle (converged)
```

### 2.2 K8s path — `talosctl upgrade-k8s` (Talos-orchestrated, Medea-monitored)

`upgrade-k8s` is a single cluster-wide operation Talos runs itself (it
sequences control-plane components and kubelets). Medea does **not** drive
it node-by-node. Per PRD §8.4 this is invoked through the **imported Talos
upgrade package** (main module), never by shelling out to `talosctl` — the
one place Medea depends on Talos's heavier main module, quarantined behind a
`K8sUpgrader` interface so the dependency and its version-coupling stay
contained:

```
if cluster.role-changes needed:
    if snapshotBeforeControlPlane: take etcd snapshot first
    cluster.rollout.phase = Upgrading
    talos.upgradeK8s(from=observed.k8s, to=desired.k8s)   # long-running, Talos-driven
    poll cluster health + node kubelet versions until all == desired
        on failure / stall past timeout -> HALT (phase=Failed)
    cluster.observed.kubernetesVersion = desired
    phase = Idle
```

`maxUnavailable` does not apply here — Talos manages disruption internally.
The operator-facing knob that *does* apply is `snapshotBeforeControlPlane`
(K8s upgrades touch the control-plane components).

## 3. Safety rails (decisions)

- **`maxUnavailable` (OS path).** Default `1`. Bounds how many pool members
  are simultaneously not-Ready. Alternative considered: a percentage —
  deferred; absolute count is clearer at 3-node scale.
- **Drain respects PDBs and times out.** No `--force`, no
  `--delete-emptydir-data` by default. On timeout the rollout **halts** and
  surfaces the blocking pod rather than evicting it. Rationale: a stuck
  drain usually means a real availability constraint; forcing it is the
  operator's explicit call, not the default.
- **Halt-on-failure (the core property).** The *first* node that fails to
  drain, upgrade, or come back healthy stops the entire rollout
  (`state=Failed`, `phase=Failed`). Rationale: prevents a bad image from
  marching across the whole fleet — the single most important difference
  between this and a `for` loop of `talosctl upgrade`. Resuming is an
  explicit operator action after they investigate (`medea rollout resume`).
- **Snapshot-before-control-plane.** Mandatory default for `role:
  controlplane`. On a single-member etcd this snapshot is the only undo. If
  the snapshot fails, the rollout aborts before touching the node.
- **Pause/resume.** `paused: true` halts progression at the next safe point
  (between nodes; never mid-node). The in-flight node finishes its
  transition to `Done` or `Failed` first.

## 4. Resume-after-reboot (why state lives in the store)

The target cluster is single-master. A control-plane OS upgrade reboots the
only apiserver; a K8s upgrade restarts control-plane components. During that
window Medea's kube client gets connection errors — **expected, not an
error condition.**

Because `Machine.rollout.state` and `Cluster.rollout.phase` live in Medea's
own datastore (not on the node, not in the cluster), the reconciler is fully
resumable:

- If Medea restarts mid-rollout, on boot it reads `rollout.state` per node
  and re-enters the state machine at the recorded point.
- If the managed apiserver is unreachable, the reconciler **parks** (backs
  off and retries) rather than treating it as failure, until either the node
  returns healthy (→ continue) or a hard timeout trips (→ halt).
- Every transition is **idempotent**: re-issuing `talos.upgrade` on a node
  already at the desired version is a no-op (version check precedes the
  call); re-cordoning an already-cordoned node is a no-op.

This is the concrete reason Medea is external (PRD Appendix B): an
in-cluster controller cannot observe its own control-plane reboot because it
goes down with it.

## 5. State machine summary

```
Machine.rollout.state:
  Idle ──▶ Draining ──▶ Upgrading ──▶ WaitingHealthy ──▶ Done
             │             │               │
             └─────────────┴───────────────┴────────▶ Failed  (halts the rollout)

Cluster.rollout.phase (K8s path):
  Idle ──▶ Upgrading ──▶ Idle
                │
                └────▶ Failed
```

`Done`/`Idle` are terminal-success; `Failed` is terminal until an operator
`resume`s (which resets the failed node to `Idle` and re-evaluates).

## 6. Open questions

- **Node ordering within a pool.** Deterministic (stable by address) is
  enough for v1. A future option: drain-cost-aware ordering (fewest pods
  first). Deferred.
- **Concurrent rollouts across pools.** v1 processes one pool's rollout at a
  time per cluster to keep `maxUnavailable` reasoning simple. Parallel
  worker-pool rollouts are a later optimization.
- **Snapshot destination.** v1 stores the pre-CP snapshot via the Talos API;
  where it lands (local to Medea vs MinIO) ties into the deferred backup
  feature. For v1, persist it somewhere durable to Medea and log the
  location.

## 7. Test plan (maps to PRD §9)

- Unit (faked Talos/kube clients): version diffing; `maxUnavailable`
  accounting; halt-on-failure trips on injected unhealthy node; drain
  timeout → halt (no force); resume re-enters at recorded `rollout.state`;
  idempotent re-issue is a no-op.
- Integration (Talos-in-docker): real `upgrade-k8s` converges; real drain
  respects a PDB; etcd snapshot is taken before a CP-role change.
