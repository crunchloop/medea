---
name: domain-model
description: >-
  Domain-first checklist for non-trivial changes under medea/. Invoke BEFORE
  writing or reviewing code that adds/changes an aggregate, a reconciler, a gRPC
  handler, a store record/bucket, a domain event, a proto message, or an
  external-client (Talos/kube/creds) interaction. Maps the change onto Medea's
  bounded contexts and DDD posture, then hands off to the tactical conventions.
  Skip only for docs, comments, tests of existing behavior, or pure mechanical
  refactors with no model change.
---

# Medea domain-modeling checklist (advisory)

Medea has a deliberate architecture (anemic proto domain model, datastore as the
single source of truth, reconcile-loop services, ACLs over external systems).
This skill makes a non-trivial change reason about the **domain** before the
mechanics, so new code lands in the right bounded context and respects the right
consistency boundary. It is **advisory** — it surfaces the questions and the
right references; it does not block.

Read [`DOMAIN.md`](../../../DOMAIN.md) (strategic map + glossary + jargon
decoder) once before answering. The detail lives in
[`design/aggregates/`](../../../design/aggregates/README.md) and the decision
records in [`design/`](../../../design/README.md).

## Step 1 — Locate the change (which context?)

State which bounded context the change belongs to. If it spans more than one,
say so explicitly — that is usually a sign to split the change.

- **Version Rollout (core)** — `internal/rollout`; rollout/safety handlers in
  `internal/server`. Aggregates: `Rollout`, `MachineRollout`, `ClusterRollout`.
- **Cluster Inventory (supporting)** — `internal/seed`, `internal/refresh`;
  inventory handlers in `internal/server`. Aggregates: `Cluster`, `NodePool`,
  `Machine`.
- **Persistence + Shared Kernel (generic)** — `internal/store`, `gen/medea/v1`.
- **Talos / Kubernetes Integration (generic ACLs)** — `internal/talos`,
  `internal/kube`.
- **Security & Credentials (generic)** — `internal/creds`, `internal/tlsgen`,
  `internal/auth`.

## Step 2 — Aggregate & consistency boundary

- Which aggregate (record) does this touch or introduce? What is its key
  (`cluster` / `cluster/pool` / `cluster/addr`)?
- The consistency boundary is **one record / one write**. There is no
  cross-aggregate transaction — if your change seems to need one, model it as
  eventual consistency driven by a reconciler instead.
- **Writer ownership** (don't cross it): API handlers own `desired/` (CAS);
  reconcilers own `rollouts/` (LWW); refresh owns the in-memory observed
  projection. Never write observed to bbolt.

## Step 3 — Ubiquitous language

- Is the term already in the `DOMAIN.md` glossary? Reuse the exact word.
- Check the **jargon decoder** for overloaded terms before naming anything —
  especially "rollout" (job vs MachineRollout vs ClusterRollout vs the act) and
  the four state-ish enums (`ClusterMode` / `RolloutJobState` / `RolloutState` /
  `ClusterRolloutPhase`). Don't invent a synonym for an existing concept.

## Step 4 — Invariants

- What must stay true? Write each new invariant as **what holds → what enforces
  it → why**, the way `design/aggregates/*.md` do.
- Are you preserving the safety chain? `rolloutsEnabled` (checked at create *and*
  execute) → `mode == manual` → pool `paused` → halt-on-failure →
  snapshot-before-control-plane. If your change touches any of these, the
  enforcement must stay doubled where it is today.

## Step 5 — Events (who needs to know?)

- Does this change require a new `store.Event` kind, or does it reuse one
  (`cluster`, `nodepool`, `machine`, `machine_rollout`, `cluster_rollout`,
  `rollout_job`)? Events are thin (`{kind, key, revision}`); clients re-fetch.
- Don't reach into another context with a direct call when a watched event is
  the right seam.

## Step 6 — Posture check (don't fight the architecture)

- **No behavior on proto types** — put logic in a handler (application service)
  or reconciler (domain service), not on `pb.*`.
- **No second source of truth** — the store is canonical; observed is
  re-derivable; no file/cache trusted as truth.
- **External dependency → behind a small interface** in an ACL package; never
  imported directly by a reconciler.
- **Time at the edge** — stamp timestamps in handlers, keep the reconcile core
  deterministic.

## Step 7 — Hand off to tactical conventions

Once the domain questions are answered, proceed with the implementation per the
relevant decision record:
[`datastore.md`](../../../design/datastore.md),
[`api-and-auth.md`](../../../design/api-and-auth.md),
[`talos-client.md`](../../../design/talos-client.md),
[`rollout-controller.md`](../../../design/rollout-controller.md),
[`rollout-safety.md`](../../../design/rollout-safety.md). Update the affected
`design/aggregates/*.md` record if the change alters an invariant, command, or
event.
