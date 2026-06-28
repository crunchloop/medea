# Medea — agent & contributor guide

Medea is an **external control plane for operating Talos clusters** (safe
version rollouts in v1). It is a standalone Go service that does **not** run
inside, or depend on, the cluster it manages. See [`PRD.md`](PRD.md) for scope
and rationale.

## Read these first

- [`DOMAIN.md`](DOMAIN.md) — the strategic map: bounded contexts, the DDD
  posture (how DDD maps onto this repo's deliberate choices), the ubiquitous
  language glossary, and a jargon decoder for overloaded terms ("rollout" means
  four things; there are four different state enums).
- [`design/aggregates/`](design/aggregates/README.md) — per-aggregate records:
  consistency boundary, lifecycle, invariants (each tied to its enforcing
  command and the *why*), command/event surface.
- [`design/`](design/README.md) — the topic decision records (store, api/auth,
  talos-client, rollout-controller, rollout-safety).

## Domain-first discipline

Before a **non-trivial change** under `medea/` — anything that adds or changes
an aggregate, a reconciler, a gRPC handler, a store record/bucket, a domain
event, a proto message, or a Talos/kube/creds interaction — **invoke the
`domain-model` skill** (`.claude/skills/domain-model/`). It runs a short
domain-first checklist (which context? which consistency boundary? is the term
already in the glossary? what invariants? who needs to know → event?) and then
hands off to the tactical conventions above. It is advisory, not a gate.

Skip it for docs, comments, tests of existing behavior, or pure mechanical
refactors with no model change.

## Architectural guardrails (the short version)

- **Anemic domain model is intentional.** The proto types in `gen/medea/v1` are
  pure data; behavior lives in handlers (application services) and reconcilers
  (domain services). Don't add methods to `pb.*` types.
- **The datastore is the single source of truth.** Desired state is precious
  (persisted, CAS); observed state is a rebuildable in-memory cache — never
  persist it, never trust a file as truth.
- **Writer separation:** API handlers own `desired/`; reconcilers own
  `rollouts/`; refresh owns observed. Don't cross these.
- **External systems sit behind small interfaces** (ACLs in `talos`/`kube`);
  reconcilers never import upstream types.
- **The rollout safety chain is load-bearing:** `rolloutsEnabled` (default off,
  checked at create *and* execute) → `mode == manual` → pool `paused` →
  halt-on-failure → snapshot-before-control-plane. Preserve every layer.
