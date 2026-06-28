# Aggregate design records — the invariant catalog

Per-aggregate design records for Medea's load-bearing domains. Each record
describes *shipped code* through a domain lens: consistency boundary,
lifecycle/state machine, invariants (each tied to the command that enforces it
and *why*), command surface, event surface, cross-context dependencies, and key
decisions.

These complement, and do not duplicate, the topic-oriented **decision** records
one level up in [`../`](../README.md) (store, api-and-auth, talos-client,
rollout-controller, rollout-safety) and the strategic map in
[`../../DOMAIN.md`](../../DOMAIN.md). When a record disagrees with the code, the
code wins.

## Catalog

| Aggregate | Context | Root type | Consistency boundary | Headline invariant | Record |
| --- | --- | --- | --- | --- | --- |
| **Rollout** (job) | Version Rollout (core) | `pb.Rollout` | one record per `cluster/pool` | no job without `rolloutsEnabled` + `manual` mode (checked twice) | [`rollout.md`](rollout.md) |
| **MachineRollout** | Version Rollout (core) | `pb.MachineRollout` | one record per `cluster/addr` | halt-on-failure; snapshot-before-control-plane; idempotent resume by re-derivation | [`machine-rollout.md`](machine-rollout.md) |
| **ClusterRollout** | Version Rollout (core) | `pb.ClusterRollout` | one record per `cluster` | snapshot-before-K8s mandatory; trigger-once-and-verify; cluster-wide only | [`cluster-rollout.md`](cluster-rollout.md) |
| **Cluster** | Cluster Inventory | `pb.Cluster` | one record per `cluster` | desired is precious/CAS; observed is in-memory only; `rolloutsEnabled` never set by seed | [`cluster.md`](cluster.md) |
| **NodePool** | Cluster Inventory | `pb.NodePool` | one record per `cluster/pool` | `desired.talosVersion == ""` inherits the cluster; strategy carries the safety knobs | [`nodepool.md`](nodepool.md) |
| **Machine** | Cluster Inventory | `pb.Machine` | one record per `cluster/addr` | identity = Talos endpoint; observed never persisted | [`machine.md`](machine.md) |

All six load-bearing aggregates are documented. `cluster-rollout.md` describes a
**deferred** (M3) aggregate — type and storage exist, no reconciler drives it
yet — and is marked as such throughout.

## How to read an invariant row

Every invariant in a record is written as **what must hold → what enforces it →
why**. The "why" is the part that isn't in the code; the "what enforces it"
points at the exact handler/reconciler so you can verify it still holds before
relying on it.
