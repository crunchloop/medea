# Aggregate: ClusterRollout (Kubernetes-path phase)

**Context:** Version Rollout (core domain) · **Type:** aggregate (reconciler-owned) ·
**Status:** Implemented (M3) — wired end-to-end (API → executor → reconciler),
unit-tested, and the underlying `upgrade-k8s` primitive validated on a docker
Talos cluster (`TestK8sUpgrade`).

`ClusterRollout` tracks the **cluster-wide Kubernetes upgrade phase**. Unlike the
OS path (which Medea drives node-by-node via [`MachineRollout`](machine-rollout.md)),
`upgrade-k8s` is **orchestrated by Talos itself**; Medea *triggers* it once and
*verifies* convergence. This record holds that monitoring state.

- **Type:** `pb.ClusterRollout` ([`gen/medea/v1`](../../gen/medea/v1))
- **Storage:** `bRollouts/clusters` (`store.PutClusterRollout`/`GetClusterRollout`, key = cluster)
- **Authorized by:** a `Rollout` job with `kind = KUBERNETES`, cluster-wide
  (`pool == ""`) — `server.CreateRollout` → `createKubernetesRollout` ([`internal/server`](../../internal/server))
- **Driven by:** `rollout.Reconciler.ReconcileK8s` ([`internal/rollout`](../../internal/rollout)), invoked by `rollout.Executor` for K8s jobs
- **Upgrade primitive:** the quarantined `talos.K8sUpgrader` /
  [`internal/talos/k8supgrade`](../../internal/talos/k8supgrade), injected into the executor via `WithK8sFactory` from `cmd/medea` (`talos-client.md` §4)
- **Read by:** `server.GetRollout` (returned in `rollout status`)

## Consistency boundary

- **One record per cluster.** Reconciler-owned, last-writer-wins (single owner),
  consistent with the OS-path progress records (`datastore.md` §6).
- Separate from the [`Cluster`](cluster.md) aggregate (which holds desired/
  observed K8s *version*); this record holds the *phase of an in-flight upgrade*.

## Lifecycle / state machine

`ClusterRolloutPhase`, driven by `ReconcileK8s` (`rollout-controller.md` §2.2, §5):

```
  Idle ──▶ Upgrading ──▶ Idle        (success / already-converged)
                │
                └────▶ Failed        (snapshot, upgrade, or convergence failure — halts)
```

`ReconcileK8s` sets `Upgrading`, reads the control-plane node's current kubelet
version as `from`, takes a mandatory etcd snapshot, calls `UpgradeK8s(from,to)`
(blocks to completion), verifies every node reports the target, then sets `Idle`.
Any failure routes through `failCluster` → `Failed`.

`maxUnavailable` does **not** apply (Talos manages disruption internally); the
gate that does is the mandatory etcd snapshot.

## Invariants

| # | Invariant | Enforced by | Why |
| --- | --- | --- | --- |
| I1 | **An etcd snapshot precedes the upgrade; snapshot failure aborts before any mutation.** | `ReconcileK8s` (`r.snapshot(cpIP)` before `k8s.UpgradeK8s`; `failCluster` on error) | The K8s upgrade mutates control-plane components; on single-member etcd the snapshot is the only undo (`rollout-safety.md` §4). Confirmed by `TestReconcileK8sHaltsOnUpgradeFailure` (snapshot taken even when the upgrade fails). |
| I2 | **Medea triggers once and verifies convergence; it does not drive node-by-node.** | `ReconcileK8s` (one `UpgradeK8s` call, then a `ListNodes` kubelet-version check) | `upgrade-k8s` is Talos-orchestrated by design (`talos-client.md` §4, PRD §8.3). |
| I3 | **The heavy Talos main-module import is quarantined to one package; `internal/rollout` never imports it.** | `internal/talos/k8supgrade` behind `talos.K8sUpgrader`; the reconciler depends on the local `rollout.K8sOps` interface; the concrete impl is injected via `Executor.WithK8sFactory` from `cmd` | Contains version-coupling to one seam and keeps the reconciler unit-testable with a fake (`talos-client.md` §4, §7; PRD §13 #15). |
| I4 | **The same enable/manual/job guards apply as the OS path.** | `CreateRollout` (cluster exists, `rolloutsEnabled`, `mode == manual`, valid target) + `Executor` re-checks `rolloutsEnabled` at execution | The safety model is path-independent (`rollout-safety.md`; cross-ref [`rollout.md`](rollout.md) I1–I4). |
| I5 | **A K8s rollout is cluster-wide; a pool is rejected.** | `createKubernetesRollout` (`InvalidArgument` if `pool != ""`) | Talos upgrades the whole cluster; a per-pool K8s rollout is meaningless. Confirmed by `TestCreateK8sRolloutRejectsPool`. |
| I6 | **An already-converged cluster is a no-op** (no snapshot, no upgrade). | `ReconcileK8s` (`sameVersion(from, target)` early return) | Idempotent / resume-safe re-runs. Confirmed by `TestReconcileK8sSkipsWhenConverged`. |
| I7 | **Without a K8s upgrader wired, a K8s job is refused, not silently skipped.** | `Executor.runJob` (`k8sFactory == nil` → job `Failed`) | Prevents a half-enabled K8s path. Confirmed by `TestExecutorRefusesK8sWithoutUpgrader`. |

## Command surface

| Command (gRPC) | CLI | Effect |
| --- | --- | --- |
| `CreateRollout` (`kind = KUBERNETES`, no pool) | `medea upgrade --cluster … --k8s <v> [--confirm]` | Sets `Cluster.desired.kubernetesVersion` and creates a cluster-wide `Rollout` job (I4/I5). Without `--confirm` the CLI only prints a plan. |
| `GetRollout` | `medea rollout status --cluster …` | Returns the `ClusterRollout` (phase + target). |

## Event surface

- `cluster_rollout` — emitted on every phase transition (`setClusterPhase`/`failCluster` → `PutClusterRollout`).
- `rollout_job` — the owning job's state (`Pending → Running → Done/Failed`).

## Cross-context dependencies

- **Cluster Inventory:** `createKubernetesRollout` writes `Cluster.desired.kubernetesVersion`; the reconciler reads the control-plane node's current kubelet version (the `from`).
- **Kubernetes Integration (ACL):** `KubeOps.ListNodes` (discover the control-plane node, read/verify kubelet versions).
- **Talos Integration (ACL):** `TalosOps.EtcdSnapshot` (the snapshot gate) + the quarantined `K8sUpgrader` (the upgrade itself).
- **Persistence + Shared Kernel:** `store.PutClusterRollout`; `pb.ClusterRollout` is kernel.

## Key decisions

- **Two upgrade paths, two aggregates** — OS = Medea-driven per-node
  (`MachineRollout`); K8s = Talos-orchestrated cluster-wide (`ClusterRollout`)
  (PRD §8.3, `rollout-controller.md` §2).
- **Snapshot-before-K8s is mandatory** (I1) — symmetric with the OS
  control-plane gate; a K8s upgrade always touches control-plane components.
- **Quarantine via injection** (I3) — the concrete `k8supgrade` is wired only at
  `cmd` (`WithK8sFactory`), so `internal/rollout` and its unit tests stay free of
  the Talos main module.
- **Known gaps:** `from` is read live from the control-plane node at reconcile
  time (not from stored observed); convergence is a single post-upgrade
  `ListNodes` check (the `UpgradeK8s` call already blocks to completion) rather
  than a long poll loop; control-plane *OS* rollout resume-after-reboot is still
  validated on QEMU for a worker only.
