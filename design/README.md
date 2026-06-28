# Design records

This directory holds the architectural design notes for `medea` — the
external control plane for operating Talos clusters — capturing the
choices made before each major piece of code landed, why, and the
trade-offs accepted. They are written for contributors who want to
understand *why* the code looks the way it does, not as user-facing
documentation. For the overall plan and scope, see [`PRD.md`](../PRD.md).

The docs reflect the state of the world at the time they were written.
They are not kept perfectly in sync with the code; when a record
disagrees with the code, the code is authoritative. Sections that
explicitly call out alternatives or "future work" are kept because the
reasoning stays useful after the work ships.

## Documents

| File | Topic |
| --- | --- |
| [`datastore.md`](datastore.md) | The embedded (bbolt) store: desired-vs-observed schema, proto-as-storage, revisions, watch, concurrency, crash recovery, desired-state export. |
| [`api-and-auth.md`](api-and-auth.md) | The gRPC service (intent verbs, thin watch), v1 auth (bearer token over TLS), and credential storage (separate from bbolt). Resolves #8/#9. |
| [`talos-client.md`](talos-client.md) | Talos & kube clients (no shelling): small Medea interfaces over the `machinery` client (OS upgrade/snapshot/health/version) + client-go (readiness/drain); the quarantined main-module `upgrade-k8s`; installer-image/schematic derivation. |
| [`rollout-controller.md`](rollout-controller.md) | The v1 version-rollout reconciler: per-node state machine, OS vs K8s paths, halt-on-failure, resume-after-reboot, control-plane snapshot safety. (Trigger in §1 superseded by `rollout-safety.md`.) |
| [`rollout-safety.md`](rollout-safety.md) | How rollouts are triggered + the guards making accidental action impossible: per-cluster `mode` (manual default), `rolloutsEnabled` (default off), plan/confirm, boot safety. Reverses rollout-controller.md §1. |
| [`provisioning-plane.md`](provisioning-plane.md) | **(v2, Draft for review)** Layer-0: the `Host` inventory aggregate + `NodePool` replicas/selector, the Matchbox driver, spec-based machine-config generation, Image-Factory schematic resolution, secrets capture, and the join-existing-cluster reconciler. Power-agnostic (the `Power` interface is a v4 seam). |

These records are **decision-oriented** (why each subsystem looks the way it
does). For the **domain lens** — bounded contexts and the strategic map, see
[`../DOMAIN.md`](../DOMAIN.md); per-aggregate invariant/lifecycle records, see
[`aggregates/`](aggregates/README.md).

Planned design records (to be written as work approaches):

- `backup.md` — (v3) the backup scheduler + restore flow (etcd snapshot
  schedule/retention/destination, and the restore that control-plane auto-repair
  needs). The v1 ad-hoc pre-mutation snapshot is its seed.
- `auto-repair.md` — (v4) failure detection + the `Power` driver
  (WoL/smart-plug/Redfish); reprovision a dead node. Builds on the provisioning
  plane and restore.

## What's *not* here

- Code-level API documentation lives next to the code as Go doc comments.
- Upstream specs (Talos machine API, `talosctl upgrade-k8s` semantics, COSI) live at their canonical URLs (see PRD Appendix A). These records describe *our* implementation choices against them.
- In-flight planning, milestone trackers, and scratch notes are under `design/private/` (gitignored).

## Status legend

Each document opens with a `**Status:**` line:

- **Draft for review** — written before the work landed; reflects the design as proposed.
- **Accepted** — decided, code matches.
- **Implemented** — written before the work landed; code now matches and the document is the architectural record.

When a section is superseded by a later decision, the later document is linked from an "Appendix" at the bottom of the older one.
