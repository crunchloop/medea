# Rollout safety model

**Status:** Draft for review
**Date:** 2026-06-26

Scope: how a rollout is *triggered*, and the guards that make it impossible to
touch a cluster by accident. This **reverses** the trigger stance of
`rollout-controller.md §1` (which had the reconciler act on desired↔observed
drift). The reconciler itself (the executor) is unchanged; what changes is what
is allowed to invoke it. Blocks the "wire the reconciler to run" milestone.

## 1. The risk

Once a rollout trigger is wired, the running Medea server holds **live admin
credentials** to managed clusters and owns destructive primitives (`UpgradeOS`,
`Drain`, `EtcdSnapshot`). The design requirement is therefore not "can it
upgrade" but: **every path by which a cluster gets mutated must be a deliberate
human act.** Accidental action must be structurally impossible, not merely
unlikely. The primary managed cluster must never be rolled out
unintentionally.

## 2. Trigger model: explicit jobs, with a per-cluster mode

`desired` is a **passive record** of the intended version; **nothing acts on it
automatically** in v1. A rollout is an explicit **`Rollout` job** that an
operator deliberately creates; the reconciler only ever executes pending jobs,
never raw drift.

A per-cluster **`mode`** governs this:

- **`manual` (default, only mode implemented in v1):** rollouts happen *only*
  via an explicit, confirmed `Rollout` job. Editing `desired` alone is inert.
- **`auto` (architected, deferred):** drift-reconcile — the reconciler converges
  `desired → observed` automatically. A future opt-in, per cluster, **never for
  control-plane pools**, and only on an enabled cluster. Not built in v1;
  selecting it is rejected.

The production cluster stays `manual`.

## 3. Guards (defense in depth)

Independent layers, so no single mistake suffices:

1. **`rolloutsEnabled` per cluster — default `false`.** Seeding *never* sets it.
   A cluster is actionable only after a separate, deliberate
   `medea cluster enable-rollouts <name>`. **The production cluster is simply never enabled**, which
   makes accidental action structurally impossible regardless of every other
   layer. Enforced at **both** job creation *and* job execution (a hand-injected
   job won't run against a disabled cluster).
2. **Explicit job (manual mode).** Setting `desired` does not act; only a
   `Rollout` job does.
3. **Plan-then-confirm.** `medea upgrade` defaults to a **dry-run plan** (nodes,
   current→target, derived image, "will snapshot CP"); it creates nothing
   without `--confirm`.
4. **Boot safety.** On startup the reconciler **resumes an already-in-flight
   job** (resume-safety, rollout-controller.md §4) but **never starts a new
   rollout from drift**. A restart cannot surprise-upgrade.
5. **Kill switch.** `medea rollout pause|abort` stops at the next safe point —
   between nodes, never mid-node (rollout-controller.md §3). `NodePool.paused`
   still applies.
6. **Audit.** Every job records `createdBy`, `createdAt`, scope, and target.

## 4. Control plane

Per the decision, control-plane pools get **no extra confirmation flag** — they
go through the same plan/confirm path as workers. The existing
**snapshot-before-control-plane** gate (rollout-controller.md §3, reconciler
`upgradeNode`) **remains mandatory**: a CP node is never mutated without a
fresh etcd snapshot first. Rationale for no extra gate: the operator selects the
pool explicitly, the plan shows the role and the snapshot step, and the snapshot
is the real safety net.

## 5. Data-model additions

```
Cluster (additions):
  mode: manual | auto          # default manual; auto rejected in v1
  rolloutsEnabled: bool        # default false; never set by seed

Rollout (new — the job / intent):
  cluster: string
  scope: { pool: string }      # or cluster-wide for the K8s path (M3)
  kind: talos | kubernetes
  targetVersion: string
  state: Pending | Running | Paused | Failed | Done | Cancelled
  createdBy: string
  createdAt: <stamped at the API boundary; the reconcile core stays time-free>
  plannedTargets: [addr...]    # machines captured at plan time — can't silently expand
```

`MachineRollout` / `ClusterRollout` remain **progress** tracking, now under an
owning `Rollout` job. A confirmed `medea upgrade` both sets `desired = target`
*and* creates the `Rollout` job; in manual mode the desired-set alone is inert.

## 6. Enforcement points

| Action | Refused unless |
| --- | --- |
| create `Rollout` job (API) | cluster exists, `rolloutsEnabled`, `mode == manual`, valid target |
| reconciler executes a job | `rolloutsEnabled` re-checked (defense in depth), job is `Pending`/`Running` |
| server boot | only resumes non-terminal jobs; never creates one |
| `mode == auto` selected | rejected in v1 (not implemented) |

## 7. CLI surface

```bash
medea cluster enable-rollouts home      # deliberate, separate from seed
medea cluster disable-rollouts home

medea upgrade --cluster home --pool workers --talos v1.13.6   # PLAN (dry-run), creates nothing
medea upgrade --cluster home --pool workers --talos v1.13.6 --confirm   # sets desired + creates job

medea rollout list   --cluster home
medea rollout status --cluster home [--pool workers] [-w]
medea rollout pause  --cluster home --pool workers
medea rollout abort  --cluster home --pool workers
```

A plan against a cluster that isn't `rolloutsEnabled` still *prints* (read-only),
but `--confirm` is refused with a clear message pointing at `enable-rollouts`.

## 8. Decisions

1. **Trigger = explicit jobs; per-cluster `mode` (manual default).** Reverses
   rollout-controller.md §1. `auto` (drift-reconcile) deferred, never CP.
2. **`rolloutsEnabled` default false**, never set by seed, enforced at create +
   execute. The hard guard for the live cluster.
3. **Plan-then-`--confirm`** for any mutating upgrade.
4. **Control plane: no extra gate**, but snapshot-before-CP stays mandatory.
5. **Boot never starts a rollout from drift** — resume in-flight jobs only.

## 9. Test plan

- Unit: job creation refused when `!rolloutsEnabled` / `mode != manual` /
  unknown cluster / bad target; reconciler refuses a job on a disabled cluster;
  boot resumes a Running job but does not create one from drift; `auto` rejected.
- Integration (scratch cluster): enable-rollouts → plan → confirm → (worker)
  rollout executes; abort stops between nodes. (UpgradeOS itself still validated
  on qemu/hardware, not docker.)
