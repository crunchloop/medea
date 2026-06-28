# Aggregate: Rollout (the job)

**Context:** Version Rollout (core domain) · **Type:** aggregate root ·
**Status:** Implemented (Talos/OS path; Kubernetes path deferred to M3)

The `Rollout` job is the **authorization-and-tracking** aggregate of the core
domain: the explicit, audited record that permits and follows a single upgrade.
In manual mode (v1), **nothing upgrades without one** — editing desired state
alone is inert (`design/rollout-safety.md` §2).

This record is *descriptive of shipped code*. It points at symbols, not line
numbers. For the prose rationale see [`PRD.md`](../../PRD.md) §8,
[`design/rollout-safety.md`](../rollout-safety.md), and
[`design/rollout-controller.md`](../rollout-controller.md); for the strategic
map see [`DOMAIN.md`](../../DOMAIN.md).

- **Type:** `pb.Rollout` ([`gen/medea/v1`](../../gen/medea/v1))
- **Created/authorized by:** `server.Server.CreateRollout` → `createTalosRollout` ([`internal/server`](../../internal/server))
- **Executed by:** `rollout.Executor` ([`internal/rollout`](../../internal/rollout))
- **Per-node execution it drives:** the `MachineRollout` aggregate (see
  [`machine-rollout.md`](machine-rollout.md)) via `rollout.Reconciler.ReconcilePool`

## Consistency boundary

- **One record = one aggregate.** Persisted in the store's jobs bucket, keyed
  `cluster/pool` (`store.PutRolloutJob`, `GetRolloutJob(cluster, pool)`). There
  is **at most one job per (cluster, pool)** — a new job for the same pool
  overwrites the prior job record (LWW, reconciler/API-owned). Cluster-wide K8s
  jobs (empty pool) are the deferred M3 shape.
- **The job does not transactionally contain the per-node progress.** It
  *governs* the pool's `MachineRollout` records, but those are separate
  aggregates with their own writes (`design/datastore.md` §6 writer separation).
  Cross-record consistency is eventual, achieved by the reconciler re-running
  (`ReconcilePool` is idempotent). Do not assume a single transaction spans the
  job and its machines.
- **Writer ownership:** the API writes the job on creation (`Pending`); the
  executor owns all subsequent state transitions (`Running` → terminal).

## Lifecycle / state machine

`RolloutJobState`:

```
            CreateRollout (guards pass)
                  │
                  ▼
              Pending ──Executor.runJob──▶ Running ──ReconcilePool ok──▶ Done
                  │                           │
                  │                           └──ReconcilePool err / bad kind──▶ Failed
                  │
        (boot resume: a job left Running is re-driven — RunOnce, idempotent)
```

- `actionable()` = `Pending` or `Running`. Only those are picked up by
  `Executor.RunOnce`.
- **Boot resume:** on startup `RunOnce` re-drives any `Running` job (the per-node
  state machine resumes from its recorded `MachineRollout.state`); it **never**
  creates a job from desired↔observed drift (`design/rollout-safety.md` §3 #4).
- `Paused` / `Cancelled` are defined in the enum but **not yet set by any v1
  command** — pause is currently expressed on the *pool* (`NodePool.paused`),
  which makes the reconciler stop between nodes. Wiring job-level pause/cancel is
  future work; note the gap rather than assuming it works.

## Invariants

Each invariant names the command/code that enforces it and *why* it exists.

| # | Invariant | Enforced by | Why |
| --- | --- | --- | --- |
| I1 | **No job is created against a cluster that isn't `rolloutsEnabled`.** | `CreateRollout` (returns `FailedPrecondition` pointing at `enable-rollouts`) | The hard guard that makes accidental action on the live production cluster *structurally* impossible — it is simply never enabled (`rollout-safety.md` §3 #1). |
| I2 | **The enabled-guard is re-checked at execution**, not trusted from the stored job. | `Executor.RunOnce` (skips clusters where `!rolloutsEnabled`) **and** `Executor.runJob` (re-checks before acting) | Defense in depth: a hand-injected or stale job must not run against a since-disabled cluster (`rollout-safety.md` §6). |
| I3 | **Only `manual` mode acts; `auto` is refused.** | `CreateRollout` (returns `Unimplemented` for `CLUSTER_MODE_AUTO`) | Drift-reconcile must never auto-mutate a cluster in v1 (`rollout-safety.md` §2). |
| I4 | **Only Talos/OS rollouts run in v1; Kubernetes is refused.** | `CreateRollout` (`Unimplemented` for `KUBERNETES`) and `runJob` (marks the job `Failed` if it sees a non-Talos kind) | The K8s path (the quarantined heavy import) lands in M3; refuse rather than half-run it. |
| I5 | **A Talos job requires an existing pool and a non-empty target.** | `CreateRollout` / `createTalosRollout` (`InvalidArgument` / `NotFound`) | A job must name a real consistency target; no cluster-wide Talos upgrades. |
| I6 | **Authorizing a job also sets the pool's desired version** (`NodePool.desired.talosVersion = target`). | `createTalosRollout` (CAS write before recording the job) | The reconciler converges to *desired*; the job and the desired version are set together under the same guards so they can't diverge. |
| I7 | **The blast radius is captured at plan time** (`PlannedTargets = NodePool.members` at creation). | `createTalosRollout` | The set of machines a job may touch can't silently expand if membership changes later (`rollout-safety.md` §5). |
| I8 | **Audit fields are stamped at the API boundary** (`CreatedBy`, `CreatedAt` RFC3339). | `createTalosRollout` (`time.Now()`) | The reconcile core stays time-free/deterministic for testing; time enters only at the edge (`datastore.md` §10, `rollout-safety.md` §5). |
| I9 | **Boot never starts a rollout from drift** — only non-terminal jobs resume. | `Executor.RunOnce` (acts only on `actionable()` jobs) | A restart must not surprise-upgrade (`rollout-safety.md` §3 #4). |

Safety properties that this job *delegates* to the per-node machine (see
[`machine-rollout.md`](machine-rollout.md)): halt-on-failure,
snapshot-before-control-plane, PDB-respecting drain with no force, and
`maxUnavailable`. A failed `MachineRollout` surfaces as a `ReconcilePool` error
that drives this job to `Failed` (I-chain from node to job).

## Command surface

| Command (gRPC) | CLI | Effect on this aggregate |
| --- | --- | --- |
| `EnableRollouts` / `DisableRollouts` | `medea cluster enable-rollouts\|disable-rollouts <cluster>` | Flips the `Cluster.rolloutsEnabled` precondition for I1/I2 (an Inventory write, but the gate for this aggregate). |
| `CreateRollout` | `medea upgrade … --confirm` | Creates the job (`Pending`) + sets pool desired (I6). Without `--confirm` the CLI only prints a plan and creates nothing. |
| `ListRollouts` | `medea rollout list --cluster <c>` | Reads jobs for a cluster. |
| `GetRollout` | `medea rollout status` | Reads the `ClusterRollout` + the pool's `MachineRollout`s (progress view; not the job record itself). |
| `PauseRollout` / `ResumeRollout` | `medea rollout pause\|resume` | **Currently toggles `NodePool.paused`** (Inventory), which the reconciler honors between nodes — *not* a job-state change. See the lifecycle note on `Paused`. |

The executor (`Executor.Run`) is started by `medea serve` **only** when the
operator passes `--rollouts` (default off) — a third, process-level gate on top
of I1/I2.

## Event surface

- `rollout_job` — emitted on job create and every state transition (consumed by
  `Watch`, e.g. `medea rollout status -w`).
- `machine_rollout` — emitted by the per-node machine this job drives (the
  observable progress).
- `cluster_rollout` — the K8s-path phase (deferred with that path).

Events are thin (`{kind, key, revision}`); clients re-fetch (`DOMAIN.md` §6).

## Cross-context dependencies

- **Cluster Inventory** (reads): `Cluster` (`rolloutsEnabled`, `mode`,
  `endpoints`), `NodePool` (`members`, `strategy`, desired version, `paused`),
  `Machine` (`role`). **Writes** `NodePool.desired` on create (I6).
- **Persistence + Shared Kernel:** `store.Store` as repository; `pb.Rollout` is
  a shared-kernel type.
- **Talos / Kubernetes Integration (ACL):** reached only via `rollout.TalosOps`
  / `rollout.KubeOps`, built per-job by `rollout.CredsFactory`.
- **Security & Credentials:** `creds.Store` supplies the talosconfig/kubeconfig
  the factory needs; credentials never enter the job record.

## Key decisions

- **Trigger = explicit job, not drift** (`rollout-safety.md` §2, reverses
  `rollout-controller.md` §1). The job is the unit of authorization.
- **`rolloutsEnabled` default-off, enforced twice** (create + execute) — the
  decision that keeps the production cluster un-rollout-able by accident (`rollout-safety.md` §3).
- **Set-desired and create-job happen together** under one guard chain (I6), so
  there is never a desired change the operator didn't authorize via a job.
- **Time at the edge only** (I8) — keeps the reconcile core deterministic.
- **Known gaps (v1):** `Paused`/`Cancelled` job states are unused (pause lives on
  the pool); only one job per (cluster,pool) is retained (no job history yet —
  `datastore.md` §10 lists rollout history as future work); the Kubernetes path
  is refused, not implemented.
