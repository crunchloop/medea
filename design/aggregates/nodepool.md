# Aggregate: NodePool

**Context:** Cluster Inventory (supporting) · **Type:** aggregate root ·
**Status:** Implemented

`NodePool` is the managed-node-group abstraction: a set of like nodes
(`controlplane` or `workers`) that roll together. It owns the membership, the
pool-level desired Talos version, the **rollout strategy** (the safety knobs),
and the `paused` switch. It is the unit a Talos rollout targets.

- **Type:** `pb.NodePool` ([`gen/medea/v1`](../../gen/medea/v1))
- **Created by:** `seed.Apply` ([`internal/seed`](../../internal/seed))
- **Mutated by:** `server.SetNodePoolVersion`, `PauseRollout`/`ResumeRollout`,
  and `createTalosRollout` (sets desired = target) ([`internal/server`](../../internal/server))
- **Read by the core domain:** `rollout.Reconciler.ReconcilePool`

## Consistency boundary

- **One record per `cluster/pool`**, stored in `bDesired/nodepools`
  (`store.PutNodePoolDesired`/`GetNodePool`, key = `cluster\x00name`).
- **Desired + strategy + membership are persisted and written via CAS.** The
  pool has no separate observed projection — node-level observed lives on the
  member [`Machine`](machine.md) records.
- The pool *references* its members by address but does not contain them; each
  member is a separate `Machine` aggregate.

## Lifecycle

A registry record, not a state machine. Created by `seed` (one `controlplane`
pool and one `workers` pool, derived from node roles), then:

- **desired version** — set by `SetNodePoolVersion` or by `createTalosRollout`
  when a job is confirmed.
- **`paused`** — toggled by `PauseRollout`/`ResumeRollout`.
- **membership / strategy** — seeded; in v1 members are existing node addresses
  (provisioning will later make membership reconciler-managed, PRD §7.2).

## Invariants

| # | Invariant | Enforced by | Why |
| --- | --- | --- | --- |
| I1 | **An empty `desired.talosVersion` inherits the cluster's.** | `rollout.Reconciler.targetVersion` (pool `""` → `Cluster.desired.talosVersion`); `seed` writes `""` | Pool-level override is optional; most pools follow the cluster (`api-and-auth.md`, PRD §7.2). |
| I2 | **The strategy carries the safety knobs, with safe defaults applied at use.** | `Reconciler` (`maxUnavailable < 1 → 1`; `drainTimeout <= 0 → 5m`; snapshot gate only when `role == controlplane`) | A pool created without an explicit strategy still upgrades safely. |
| I3 | **`paused` halts progression between nodes, never mid-node.** | `ReconcilePool` (returns early when `paused`); the in-flight node finishes first | Pause/abort must not leave a node half-upgraded (`rollout-controller.md` §3). |
| I4 | **A job's blast radius is the pool's members captured at plan time.** | `createTalosRollout` copies `members` into `Rollout.plannedTargets` | Membership changes after authorization can't silently expand a running job (`rollout-safety.md` §5; cross-ref [`rollout.md`](rollout.md) I7). |
| I5 | **Role determines the snapshot gate and the pool's identity.** | `Reconciler.upgradeNode` (snapshot only for `ROLE_CONTROLPLANE`); `seed.poolName`/`pbRole` | Control-plane pools need the etcd-snapshot undo; workers don't. |
| I6 | **`SetNodePoolVersion` is a partial update with optional CAS.** | `server.SetNodePoolVersion` (`rmw`) | Same single-operator LWW-at-intent semantics as the cluster (`api-and-auth.md` §2). |

## Command surface

| Command (gRPC) | CLI | Effect |
| --- | --- | --- |
| `ListNodePools` | `medea get nodepools --cluster …` | Read. |
| `SetNodePoolVersion` | `medea upgrade --pool … --talos …` (without `--confirm`: plan only) | Partial update of the pool's desired Talos version. |
| `PauseRollout` / `ResumeRollout` | `medea rollout pause\|resume --pool …` | Toggles `paused` (I3). |

`createTalosRollout` (from `CreateRollout`/`medea upgrade --confirm`) also writes
`desired.talosVersion = target` as part of authorizing a job.

## Event surface

- `nodepool` — emitted on any desired write (version, `paused`).

## Cross-context dependencies

- **Version Rollout** reads `members`, `strategy`, `paused`, `role`, and desired
  version; it is the primary consumer of this aggregate.
- **Cluster Inventory:** inherits the version default from [`Cluster`](cluster.md)
  (I1); members are [`Machine`](machine.md) addresses.
- **Persistence + Shared Kernel:** `store.Store`; `pb.NodePool` is kernel.

## Key decisions

- **Two pools in v1 seeding** (`controlplane`, `workers`) derived from node role;
  arbitrary pools are a later concern.
- **`maxUnavailable` as an absolute count** (default 1), clearer than a
  percentage at 3-node scale (`rollout-controller.md` §3).
- **Strategy lives on the pool**, so safety policy is per-group, not global.
- **Members are addresses in v1** (existing nodes); when provisioning lands a
  pool gains `replicas` + a selector and membership becomes reconciler-managed
  (PRD §7.2) — a deliberate forward seam, not built.
