# API & auth

**Status:** Draft for review
**Date:** 2026-06-25

Scope: the gRPC service Medea exposes (RPCs, mutation semantics, watch),
the v1 authentication model, and where cluster credentials live. Resolves
PRD §13 decision #9 (auth) and the credential-storage question deferred from
`datastore.md` §9. Unblocks the remaining M1 work: the gRPC server and the
read-path CLI (`medea get …`).

Out of scope: the Talos/kube *clients* Medea uses to reach managed clusters
(→ `talos-client.md`); the rollout reconciler (→ `rollout-controller.md`).

## 1. Shape

One gRPC service (`medea.v1.Medea`), defined in a new
`proto/medea/v1/service.proto` that imports the existing message file. The CLI
is a thin typed client over it; a future web UI is another client. The server
process and the store live together (PRD §8.1); the API is how *clients* reach
them — it is never consumed from inside a managed cluster.

The RPCs mirror the CLI verbs (PRD §7.1) one-to-one:

```proto
service Medea {
  // --- reads ---
  rpc GetCluster(GetClusterRequest) returns (Cluster);
  rpc ListClusters(ListClustersRequest) returns (ListClustersResponse);
  rpc ListNodePools(ListNodePoolsRequest) returns (ListNodePoolsResponse);
  rpc ListMachines(ListMachinesRequest) returns (ListMachinesResponse);
  rpc GetRollout(GetRolloutRequest) returns (GetRolloutResponse);

  // --- desired-state mutations (intent verbs, not raw Put) ---
  rpc SetClusterVersions(SetClusterVersionsRequest) returns (SetVersionsResponse);
  rpc SetNodePoolVersion(SetNodePoolVersionRequest) returns (SetVersionsResponse);
  rpc PauseRollout(PauseRolloutRequest) returns (PauseRolloutResponse);
  rpc ResumeRollout(ResumeRolloutRequest) returns (ResumeRolloutResponse);

  // --- watch ---
  rpc Watch(WatchRequest) returns (stream WatchEvent);
}
```

Design notes:

- **Intent verbs, not a generic `Put`.** `SetClusterVersions` / `SetNodePoolVersion`
  express the operation (`medea upgrade --k8s …`), not "overwrite this record."
  This keeps the read-modify-write server-side (§3) and the surface aligned with
  what an operator actually does. Raw record writes stay internal to the store
  package.
- **`GetRollout`** returns both the `ClusterRollout` (K8s path) and the
  per-machine `MachineRollout`s for a pool, since that's what `rollout status`
  renders.
- **Reads return the proto domain types directly** (`Cluster` etc.) — same types
  the store holds. No DTO layer (the proto *is* the contract, datastore.md §4).

## 2. Mutation semantics (read-modify-write + optional CAS)

The store offers compare-and-swap (datastore.md §6), but raw CAS is awkward at
the CLI. So the API mutations are **server-side read-modify-write** by default,
with optional CAS for the careful path:

```
SetClusterVersions(cluster, talos_version?, kubernetes_version?, expected_revision?):
  read current Cluster (NotFound if absent)
  if expected_revision set and != current.revision -> FailedPrecondition
  apply the provided fields to desired (omitted fields untouched)
  PutClusterDesired(updated, current.revision)   # CAS against what we just read
  on ErrConflict (concurrent writer raced us) -> retry once, else Aborted
```

Design notes:

- **Default is last-writer-wins at the *intent* level**, which is correct for a
  single-operator homelab: "set k8s to v1.36.2" shouldn't fail because someone
  changed an unrelated field. The internal `PutClusterDesired` still uses CAS to
  stay race-safe; the server retries once on a lost race.
- **`expected_revision` is opt-in** (CLI `--if-revision N`, automation can pin
  it). When set, a concurrent change surfaces as `FailedPrecondition` instead of
  being silently merged — the safety valve for scripted callers.
- **Partial updates:** unset version fields are left as-is, so `--talos` and
  `--k8s` are independent.

## 3. Watch (thin events)

`Watch` streams `WatchEvent{kind, key, revision}` — the same shape as the store's
internal `Event` (datastore.md §5). Events are **thin**: they name what changed,
not the new object.

```proto
message WatchRequest { uint64 since_revision = 1; }
message WatchEvent {
  string kind = 1;       // "cluster" | "nodepool" | "machine_rollout" | ...
  string key = 2;
  uint64 revision = 3;
}
```

Design notes:

- **Client re-fetches on event.** `rollout status -w` gets an event, then calls
  `GetRollout` for the current picture. Rationale: keeps the server stream a
  trivial pass-through of the store broadcaster; avoids embedding/serializing
  large objects into every event; the client decides what it actually needs.
- **`since_revision`** wires straight to `store.Watch(ctx, since)` — snapshot
  then live, no gap/dup. A reconnecting client passes its last-seen revision.

## 4. Authentication (resolves #9)

**v1 = bearer token over TLS.** Decided against the PRD's looser "token on a
trusted network" lean because a bearer token on *plaintext* gRPC is sent in the
clear on the LAN — cheap to do better:

- **Transport: TLS**, server presents a cert. v1 ships a self-signed cert
  generated on first run; the client trusts it via a pinned CA file
  (`--ca` / client config). Not a public CA, not Let's Encrypt — this is a
  LAN service.
- **Authn: a shared bearer token** in gRPC metadata (`authorization: Bearer
  <token>`), checked by a `UnaryServerInterceptor` + `StreamServerInterceptor`.
  Missing/wrong token → `codes.Unauthenticated`.
- **Token source:** the server config (§6) holds a token (or a path to a
  mode-0600 token file). The CLI reads it from client config / env
  (`MEDEA_TOKEN`).

Explicitly deferred to a hardening pass (architected for, not built):

- **mTLS** (client certs, Talos-style) — the natural v2; the interceptor seam
  makes it a drop-in.
- **OIDC** for human SSO and per-user identity/RBAC.
- **Authorization (RBAC).** v1 is all-or-nothing: a valid token can do anything.
  Fine for a single operator; multi-user needs roles.

This keeps v1 minimal (one token, one self-signed cert) without the
cleartext-credential footgun.

## 5. Credential storage (the datastore.md §9 deferral)

Cluster credentials — the Talos admin cert bundle (`talosconfig`) and the
`kubeconfig` Medea uses to reach managed clusters — are **not** in the bbolt
resource store. They are sensitive, and keeping them out is what makes the
desired-state export (datastore.md §9) safe to inspect/commit.

Design: a separate `CredentialStore`, behind an interface so it can evolve:

```go
type CredentialStore interface {
    TalosConfig(cluster string) ([]byte, error)   // talosconfig bytes
    KubeConfig(cluster string) ([]byte, error)     // kubeconfig bytes
    Put(cluster string, talos, kube []byte) error
}
```

- **v1 implementation: file-backed**, a directory of mode-0600 files
  (`<creds-dir>/<cluster>/{talosconfig,kubeconfig}`), `<creds-dir>` itself
  0700. The `Cluster` record references credentials by cluster name; no secret
  material is stored in or referenced from bbolt beyond that name.
- **Future:** OS keyring; an external secret store; or sourcing from
  **1Password** — the cluster already uses 1Password for DR, so a 1Password-backed
  `CredentialStore` is a natural v2 that removes plaintext creds from disk
  entirely.

The credential store is never serialized by `Export`; it has no export path.

## 6. Configuration: service config ≠ desired state

The server reads a small **service config** at startup — listen address, TLS
material, token, store path, creds dir:

```yaml
# medea.yaml  (server-side process config — NOT cluster desired state)
listen: 0.0.0.0:7600
tls:   { cert: /etc/medea/tls/cert.pem, key: /etc/medea/tls/key.pem }
token_file: /etc/medea/token
store:     /var/lib/medea/medea.db
creds_dir: /var/lib/medea/creds
```

Design note — **this is not the `cluster.yaml` we rejected** (PRD §13 #2). This
file configures the *process* (where to listen, how to auth, where its files
are); it says nothing about desired cluster state, which lives only in the
store. The distinction matters: losing/editing `medea.yaml` changes how the
daemon runs, never what it believes the clusters should be.

## 7. Error mapping

Store/domain errors map to gRPC status codes at the handler boundary:

| Condition | gRPC code |
| --- | --- |
| record absent on a Get/mutation | `NotFound` |
| `expected_revision` mismatch (opt-in CAS) | `FailedPrecondition` |
| lost race after retry (`store.ErrConflict`) | `Aborted` |
| invalid/empty required fields | `InvalidArgument` |
| missing/invalid token | `Unauthenticated` |
| (v2) token lacks permission | `PermissionDenied` |

## 8. CLI client

`medea` is a gRPC client. It resolves server address, token, and CA from (in
precedence) flags → env (`MEDEA_ADDR`, `MEDEA_TOKEN`, `MEDEA_CA`) → a client
config file (`~/.config/medea/config.yaml`). M1 ships the read verbs
(`medea get clusters|nodepools|machines`, `medea rollout status [-w]`); the
mutation verbs (`upgrade`, `rollout pause/resume`) light up with M2/M3 when
there's something to drive.

## 9. Decisions

1. **One gRPC service, intent verbs.** Surface mirrors CLI operations, not raw
   record CRUD. Raw `Put`/CAS stays in the store package.
2. **Server-side read-modify-write, opt-in CAS.** LWW-at-intent by default
   (single-operator ergonomics); `expected_revision` for scripted safety;
   internal CAS + one retry keeps it race-safe.
3. **Thin watch events; client re-fetches.** Server stream is a pass-through of
   the store broadcaster; no object embedding.
4. **Auth = bearer token over TLS (self-signed) for v1** (#9 resolved). mTLS,
   OIDC, and RBAC deferred behind the interceptor seam.
5. **Credentials in a separate file-backed `CredentialStore`, never in bbolt,
   never exported.** 1Password-backed impl is the planned v2.
6. **Service config (`medea.yaml`) is process config, distinct from desired
   state.** Not a reintroduction of the rejected `cluster.yaml`.

## 10. Test plan

- **Auth interceptor (unit):** missing token → `Unauthenticated`; wrong token →
  `Unauthenticated`; correct token → passes. Same for the stream interceptor.
- **Mutation semantics (unit, against a real `BoltStore`):** `SetClusterVersions`
  partial update leaves other fields intact; `expected_revision` mismatch →
  `FailedPrecondition`; concurrent-writer race → retried then succeeds.
- **Error mapping (unit):** `store.ErrConflict`/not-found/validation map to the
  codes in §7.
- **Watch (integration):** a client `Watch` receives snapshot then a live event
  after a mutation; reconnect with `since_revision` resumes without gap.
- **CredentialStore (unit):** files written 0600 under a 0700 dir; `Export`
  output contains no credential bytes.
