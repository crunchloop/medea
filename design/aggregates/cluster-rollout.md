# Aggregate: ClusterRollout (Kubernetes-path phase)

**Context:** Version Rollout (core domain) · **Type:** aggregate (reconciler-owned) ·
**Status:** **M3 in progress** — the `upgrade-k8s` primitive is implemented and
integration-tested behind the `K8sUpgrader` seam, but no reconciler drives this
record yet and the Kubernetes rollout path is still refused at both the API and
the executor (the remaining wiring; see "Current behavior").

`ClusterRollout` tracks the **cluster-wide Kubernetes upgrade phase**. Unlike the
OS path (which Medea drives node-by-node via [`MachineRollout`](machine-rollout.md)),
`upgrade-k8s` is **orchestrated by Talos itself**; Medea would only *trigger* it
once and *monitor* to completion. This record is the place that monitoring state
will live. It is documented now so the seam is visible; it is **not** live code.

- **Type:** `pb.ClusterRollout` ([`gen/medea/v1`](../../gen/medea/v1))
- **Storage (exists):** `bRollouts/clusters`
  (`store.PutClusterRollout`/`GetClusterRollout`, key = cluster)
- **Read by:** `server.GetRollout` (returned in the status response; currently
  nil for every cluster)
- **Will be driven by:** the K8s path of the rollout reconciler (M3), behind the
  quarantined `K8sUpgrader` interface (`talos-client.md` §4)

## Current behavior (v1)

- `CreateRollout` rejects `ROLLOUT_KIND_KUBERNETES` with `Unimplemented`
  ("kubernetes rollouts land in M3").
- `rollout.Executor.runJob` marks any non-Talos job `Failed`
  ("kubernetes rollouts not supported in v1").
- So no `ClusterRollout` record is ever written today; `GetRollout` returns it as
  nil.
- **The quarantine seam is implemented and validated** (I2, I3): the
  `talos.K8sUpgrader` interface ([`internal/talos`](../../internal/talos)) and
  its implementing package
  [`internal/talos/k8supgrade`](../../internal/talos/k8supgrade) drive Talos's
  main-module `upgrade-k8s` (the only place importing the Talos main module +
  go-kubernetes). `TestK8sUpgrade`
  (`internal/itest/k8s_upgrade_integration_test.go`) exercises a real
  v1.36.1→v1.36.2 upgrade on a docker Talos cluster to convergence.
- **What is still missing (the remaining M3 wiring):** the reconciler/executor
  do not yet *call* `UpgradeK8s`, and `CreateRollout` still refuses
  `KUBERNETES`. So although the upgrade primitive works, no `Rollout` job can
  trigger it and no `ClusterRollout` record is written yet. Wiring it (lift the
  refusals, drive the upgrader, write/track `ClusterRollout`, add the
  snapshot-before gate per I1, re-apply the safety guards per I4) is the next
  step.

## Consistency boundary (planned)

- **One record per cluster.** Reconciler-owned, last-writer-wins (single owner),
  consistent with the OS-path progress records.
- Separate from the [`Cluster`](cluster.md) aggregate (which holds desired/
  observed K8s *version*); this record holds the *phase of an in-flight upgrade*.

## Lifecycle / state machine (planned)

`ClusterRolloutPhase` (`rollout-controller.md` §2.2, §5):

```
  Idle ──▶ Upgrading ──▶ Idle
                │
                └────▶ Failed
```

- **`Upgrading`** — Medea triggers `upgrade-k8s(from, to)` (Talos-driven) and
  polls kubelet versions + cluster health to completion.
- **`Failed`** — on stall past timeout or health failure (halt).
- `maxUnavailable` does **not** apply (Talos manages disruption internally); the
  knob that does is `snapshotBeforeControlPlane` (K8s touches control-plane
  components).

## Invariants (planned)

| # | Invariant | Will be enforced by | Why |
| --- | --- | --- | --- |
| I1 | **An etcd snapshot precedes a K8s upgrade** (it mutates control-plane components). | the K8s path's snapshot gate (reusing `EtcdSnapshot`) | Same single-member-etcd undo as the OS control-plane path (`rollout-controller.md` §2.2). |
| I2 | **Medea triggers once and monitors; it does not drive node-by-node.** | the `K8sUpgrader` impl + version polling via `Kube` | `upgrade-k8s` is Talos-orchestrated by design (`talos-client.md` §4, PRD §8.3). |
| I3 | **The heavy Talos main-module import is quarantined to one package.** | `internal/talos/k8supgrade` behind the `K8sUpgrader` interface | Contains the version-coupling cost to a single seam (`talos-client.md` §4, §7; PRD §13 #15). |
| I4 | **The same enable/manual/job guards apply** as the OS path. | `CreateRollout` (once K8s is implemented) | The safety model is path-independent (`rollout-safety.md`). |

## Command surface (planned)

- `CreateRollout` with `kind = KUBERNETES` (cluster-wide; `medea upgrade --k8s …
  --confirm`) — currently `Unimplemented`.
- `GetRollout` already returns the (currently nil) `ClusterRollout` in
  `rollout status`.

## Event surface

- `cluster_rollout` — defined; will be emitted on phase transitions once the path
  is built.

## Cross-context dependencies (planned)

- **Cluster Inventory:** reads/writes `Cluster.observed.kubernetesVersion` and
  the desired K8s version.
- **Talos Integration:** the quarantined `K8sUpgrader` (main-module
  `upgrade-k8s`); **Kubernetes Integration:** kubelet-version polling for
  convergence.
- **Persistence + Shared Kernel:** `store.PutClusterRollout`; `pb.ClusterRollout`
  is kernel.

## Key decisions

- **Two upgrade paths, two aggregates** — OS = Medea-driven per-node
  (`MachineRollout`); K8s = Talos-orchestrated cluster-wide (`ClusterRollout`)
  (PRD §8.3, `rollout-controller.md` §2).
- **Refuse, don't half-run** — until M3, the K8s kind is explicitly rejected at
  both create and execute rather than partially handled.
- This record is the **monitor-to-completion** state holder, kept generic so the
  deferred backup feature can reuse the snapshot primitive.
