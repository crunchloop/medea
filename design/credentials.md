# Credentials & PKI custody — retiring `_out/`

**Status:** Draft for review
**Date:** 2026-07-04

Scope: making Medea the **durable, off-host owner** of a managed cluster's
credentials and machine-secrets bundle, so the hand-maintained `_out/` directory
in the `home-cluster` repo (talosconfig + kubeconfig + the PKI-bearing machine
configs) can be **deleted**. This is **Phase A** of the "Medea owns the PKI" goal.
Phase B — *generating* a new PKI and first-control-plane bootstrap so a rebuild is
Medea-driven — is a separate record (`cluster-bootstrap.md`, the deferred
`provisioning-plane.md` §9 flow); A is its prerequisite.

Builds on two things that already exist: v2 secrets **capture**
(`talos.CaptureSecrets` → `creds.PutSecrets`, `provisioning-plane.md` §5) and the
v1 file-backed `CredentialStore` (`api-and-auth.md` §5). Overlaps with, but is
smaller than, the v3 DR backup (`backup.md`): this is credential *custody*, not
etcd *state* recovery.

Out of scope: new-cluster creation / first-CP bootstrap (Phase B); full etcd
disaster recovery + restore (`backup.md` v3); rotation of the captured secrets
(noted §9); RBAC on the export path (rides the v1 all-or-nothing token,
`api-and-auth.md` §4).

## 1. Decisions (this pass — 2026-07-04)

1. **Off-host durability = a 1Password-backed `CredentialStore`.** The planned-v2
   impl (`api-and-auth.md` §5) becomes real: `talosconfig`, `kubeconfig`, and the
   captured `secrets.yaml` live in the same 1Password vault the cluster already
   uses for DR — off the Medea host, no plaintext on its disk. This is what makes
   `_out/` redundant (§4). Rejected-for-now: the full v3 DR bundle (bigger, and
   about etcd state, not credential custody — §8); OS keyring (still on-host).
2. **The 1Password impl uses the 1Password Go SDK + a service-account token**, not
   the `op` CLI — Medea ships on distroless (no shell/binary). The SDK **requires
   CGO** (its `!cgo` build is a hard compile error and its desktop-app path uses
   `import "C"`), so the image builds `CGO_ENABLED=1` on **distroless/base** (glibc),
   not distroless/static. A Connect sidecar (pure-Go HTTP client, CGO-free) is the
   fallback (§4.2).
3. **Secrets stay out of bbolt and out of `Export`** — unchanged invariant
   (`datastore.md` §9, `api-and-auth.md` §5). The 1Password item is the *store*,
   not a serialized export; the plaintext desired-state export remains secret-free.
4. **Client credential access via a guarded `GetCredentials` RPC** (`medea get
   credentials`), so an operator/CI keeps `kubectl`/`talosctl` access once `_out/`
   is gone (§5).
5. **`_out/` is deleted only once Medea durably holds *and can re-emit* everything
   it contained** (§4) — verified end-to-end before removal (§7 A-M3).

## 2. What `_out/` holds, and where each part goes

| `_out/` file | Contains | Home in Medea |
| --- | --- | --- |
| `talosconfig` | Talos admin client creds | `CredentialStore` (written by `seed`) |
| `kubeconfig` | k8s admin client creds | `CredentialStore` (written by `seed`) |
| `controlplane.yaml` | CP machine config **+ embedded PKI** | PKI captured as `secrets.yaml` (`capture-secrets`); the rendered config is **regenerable** from secrets + spec (`provision.RenderWorkerConfig` / a CP sibling), so not kept |
| `worker.yaml` | worker machine config + embedded PKI | same — regenerable, not kept |

**Conclusion:** once the secrets bundle is captured and the credential store is
durable + exportable, nothing in `_out/` is unique. The rendered machine configs
are derivable; the PKI and client creds live in Medea.

## 3. What already exists (the starting point)

- **`talos.CaptureSecrets(node)`** — reads the live CP node's active
  `MachineConfig` and extracts the **existing** cluster's secrets bundle
  (`secrets.yaml`); does not generate. `medea capture-secrets` stores it.
- **`creds.Store` / `FileStore`** — per-cluster `0600` files (`talosconfig`,
  `kubeconfig`, `secrets.yaml`) under a `0700` dir. Interface:
  `TalosConfig`/`KubeConfig`/`Put` + `Secrets`/`PutSecrets`.
- **`store.Export`** is secret-free (desired records only).

So Phase A adds an impl and an export path; it does **not** touch the store
aggregates, the reconcilers, or the safety chain.

## 4. 1Password-backed `CredentialStore`

A second impl of the existing `creds.Store` interface — chosen by service config,
so handlers/reconcilers that consume the interface are unchanged.

- **Layout:** one 1Password item per cluster (title `medea-<cluster>` — a hyphen,
  not a slash: `op://` references are slash-delimited, so a slash in the title is
  unresolvable), fields `talosconfig`, `kubeconfig`, `secrets.yaml`; vault
  configurable (default
  `Kubernetes`, matching the cluster's existing item conventions). One item keeps
  a cluster's material atomic; three fields avoid extra round-trips.
- **Selection (service config, `medea.yaml`):**
  ```yaml
  creds:
    backend: onepassword          # file | onepassword
    onepassword:
      vault: Kubernetes
      token_file: /etc/medea/op-token     # 1Password service-account token
  ```
- **Interface unchanged** — `TalosConfig`/`KubeConfig`/`Secrets`/`Put`/`PutSecrets`
  map to item field reads/writes. No new bbolt state; the `Cluster` record still
  references creds only by name.

### 4.1 Migration

`_out/` deletion needs the material moved into 1Password first. Either a one-shot
`medea creds migrate --from file --to onepassword --cluster <c>` (read the file
store, `Put`/`PutSecrets` into the 1Password store) or a documented manual
`op item create`. The `capture-secrets` step (§3) runs against the *live* cluster
and can write straight to the configured backend.

### 4.2 Runtime dependency + trust

The Medea orchestrator container needs a **1Password service-account token**
(mounted, `0600`, referenced by `token_file`). The token scopes to the cluster
vault; it can read/write cluster creds — same trust class as the creds
themselves, and Medea is already the top-privilege operator. The container links
the **Go SDK** (`github.com/1password/onepassword-sdk-go`) and talks to the
1Password API directly. **CGO caveat (learned the hard way):** the SDK does not
compile with `CGO_ENABLED=0` on linux/darwin — its `!cgo` file is a deliberate
hard compile error, and the desktop-app path uses `import "C"`, so the binary
links libc. The image therefore builds `CGO_ENABLED=1` and ships on
**distroless/base** (glibc), not distroless/static (`deploy/Dockerfile`). This
also breaks the old ansible `GOOS=linux CGO_ENABLED=0 go build` cross-compile —
another reason the container image (native linux build) is the deploy path.
Fallback if the CGO/SDK friction ever bites: a **1Password Connect** sidecar
(pure-Go HTTP client, CGO-free), same seam.

## 5. Client credential export (`GetCredentials`)

Because credentials live server-side (on the orchestrator), fetching them is a new
**guarded gRPC RPC**, not a local file read — so a remote operator (your laptop)
or CI can retrieve them over the authenticated TLS channel:

```proto
rpc GetCredentials(GetCredentialsRequest) returns (GetCredentialsResponse);

message GetCredentialsRequest {
  string cluster = 1;
  bool   talosconfig = 2;   // default: both client configs if neither set
  bool   kubeconfig  = 3;
  bool   secrets     = 4;   // secrets.yaml — off by default, explicit opt-in
}
```

- CLI: `medea get credentials --cluster home --kubeconfig > ~/.kube/config`
  (and `--talosconfig`). Reads from whichever backend is configured.
- **`secrets.yaml` is not emitted by default** — it is provisioning material, a
  higher sensitivity than admin client configs, and needs an explicit
  `--secrets`. (Even then, consider refusing over the wire; open question §9.)
- Guarded by the v1 bearer token (`api-and-auth.md` §4); RBAC is future.

## 6. Invariants (preserved / extended)

- **Secrets never in bbolt; `Export` stays secret-free.** Unchanged — the
  1Password item is the credential *store*, not the desired-state export.
- **Extends `api-and-auth.md` §5 decision #5:** credentials are no longer
  *only* file-backed — a 1Password-backed impl is now built. The "no serialized
  form in Export" claim still holds; that doc gets a pointer here (as `backup.md`
  §4.1 did).
- **File impl stays** the default and the test/dev backend; nothing forces
  1Password on a local run.

## 7. Milestones (Phase A)

- **A-M1 — 1Password backend.** `onepassword` `creds.Store` impl (Go SDK) +
  service-config backend switch + `creds migrate`. Unit-tested against a fake
  1Password client; `Export`-stays-secret-free test extended to the new backend.
- **A-M2 — `GetCredentials`.** Proto RPC + handler (reads the configured store) +
  `medea get credentials`. Unit (auth-guarded, field selection) + integration.
- **A-M3 — Cut over + retire `_out/`.** Capture the live cluster's secrets;
  migrate creds into 1Password; verify `medea get credentials` reproduces working
  `kubeconfig`/`talosconfig`; **then delete `_out/` from `home-cluster`** and drop
  its `secrets-{push,pull}.sh` scripts.

## 8. Relationship to other records

- **`backup.md` (v3):** the encrypted DR *bundle* (etcd + desired + secrets) is
  the fuller recovery story; this record is the credential-custody subset that
  `_out/` deletion actually needs. They compose — v3 can escrow its `age` identity
  in the same 1Password vault, and reuse this store for the secrets part.
- **`provisioning-plane.md` §9 / `cluster-bootstrap.md` (Phase B):** B *generates*
  the bundle and bootstraps the first CP; it writes into **this** `CredentialStore`.
  A is B's prerequisite — B is unsafe if the generated PKI isn't durably held.
- **`api-and-auth.md` §5:** update decision #5 (1Password impl built, not planned).

## 9. Open questions

- **1Password access shape** — Go SDK vs Connect sidecar on the orchestrator; the
  distroless image rules out the `op` CLI. Lean SDK; validate token flow.
- **Item schema** — one item with three fields vs three items per cluster.
- **Should `GetCredentials` ever emit `secrets.yaml`** over the wire, or only the
  admin client configs (secrets stay on the orchestrator, used only to render
  configs)?
- **Secret rotation** — re-capture after a cluster CA rotation; out of scope here,
  but the store must not assume immutability.
- **Token custody bootstrap** — the op service-account token is itself a secret on
  the orchestrator; it's the one credential that can't live in 1Password. (Ansible
  places it; same class as the Medea API token today.)

## 10. Test strategy (maps to PRD §9)

- **Unit (fake 1Password client):** `Put`/`Get`/`Secrets` round-trip; file →
  1Password migration; the `Export`-is-secret-free assertion holds for both
  backends; `GetCredentials` field selection + auth-guard (missing token →
  `Unauthenticated`).
- **Integration:** the SDK impl against a real service account (or Connect) —
  likely a gated CI job given it needs a live 1Password.
- **E2E (cutover rehearsal):** on a throwaway/QEMU cluster — capture → migrate →
  `get credentials` → `kubectl get nodes` works → confirm `_out/` is reproducible
  from Medea alone. Never rehearsed first on the live cluster.
