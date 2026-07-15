# medea

**An external control plane for operating Talos Linux Kubernetes clusters.**

Medea gives a Talos cluster managed-Kubernetes ergonomics — safe, observable
version rollouts — from a standalone service that runs **outside** the cluster it
operates. It does not run as Kubernetes CRDs and does not depend on the managed
cluster to function (so it survives the very outages it has to drive through). It
talks to the Talos machine API and the target kube-apiserver as an outward
client, and keeps its own datastore as the single source of truth.

- **Why this shape** (external, not in-cluster, not CRDs): [`PRD.md`](PRD.md) §1–4, Appendix B.
- **How the code is organized** (bounded contexts, glossary, aggregates): [`DOMAIN.md`](DOMAIN.md).
- **Why each subsystem looks the way it does**: [`design/`](design/README.md).

> **Status:** v1 (version rollouts). Talos OS and Kubernetes upgrades are
> implemented and tested; control-plane upgrade safety (etcd snapshot +
> resume-after-reboot) is in. Provisioning, auto-repair, and backup are
> architected-for but deferred (PRD §4, §6).

## What it does (v1)

- **Talos OS rollout** — node-by-node within a pool: cordon → drain (PDB-aware) →
  A/B upgrade + reboot → wait-healthy → uncordon, **halting on the first failure**.
- **Kubernetes rollout** — cluster-wide via Talos's own `upgrade-k8s`
  orchestration (Medea triggers and verifies convergence).
- **Safety rails** — `maxUnavailable`, drain timeout (no force),
  halt-on-failure, **mandatory etcd snapshot before any control-plane change**,
  and resume across a Medea restart or a control-plane reboot.
- **Read model** — `medea get clusters|nodepools|machines`, `medea rollout status`.

## Build

```sh
make build                      # build all packages
go build -o medea ./cmd/medea   # build the CLI/server binary
```

Requires Go (see `go.mod`). Codegen tooling (only needed to regenerate protos):
`make tools` then `make generate`.

## Run the server

The server (`medea serve`) holds the store, the API, and the reconcilers. Run it
somewhere that is **not** the managed cluster (PRD §13 #10).

```sh
medea serve \
  --listen 0.0.0.0:7600 \
  --mcp-listen 127.0.0.1:7601 \
  --store /var/lib/medea/medea.db \
  --creds-dir /var/lib/medea/creds \
  --token-file /etc/medea/token \
  --tls-cert /etc/medea/tls/cert.pem --tls-key /etc/medea/tls/key.pem \
  --rollouts                 # enable the rollout executor (global gate; default OFF)
```

Notes:

- **TLS** is generated self-signed on first run if the cert/key are missing; the
  CLI trusts it via `--ca`/`MEDEA_CA`.
- **Auth** is a shared bearer token (`--token` or `--token-file`).
- **`--rollouts` is off by default.** Even when on, a rollout still requires the
  target cluster to be individually enabled (see Safety). Leaving it off makes
  the server read-only.
- **Credentials** (talosconfig/kubeconfig) live under `--creds-dir`, **not** in
  the bbolt store, and are never exported. See [Credentials](#credentials).
- **MCP** is disabled unless `--mcp-listen` is set. It serves a curated,
  read-only Streamable HTTP endpoint at `/mcp`; bind it only to loopback or a
  trusted private network. It does not expose credential retrieval or mutation
  tools. See [`design/mcp.md`](design/mcp.md).

For a systemd unit and a container image, see [`deploy/`](deploy/).

## Iris MCP integration

Start Medea with an MCP listener reachable from Iris, then add the server to
Iris policy:

```yaml
mcpServers:
  medea:
    url: http://medea.example.internal:7601/mcp
    users: [martin]
    tools:
      - list_clusters
      - get_cluster
      - list_node_pools
      - list_machines
      - list_hosts
      - get_rollout
      - list_rollouts
    approval: never
```

The endpoint currently has no application-layer authentication. Keep it on a
private interface or place it behind an authenticated TLS reverse proxy. Its
catalog is read-only, but it still reveals cluster inventory and topology.

## Credentials

Per managed cluster, drop the admin credentials under the creds dir (mode 0600,
dir 0700):

```
<creds-dir>/<cluster>/talosconfig
<creds-dir>/<cluster>/kubeconfig
```

`medea seed` writes them there for you (below).

## Client config

The CLI is a gRPC client. It resolves, in precedence, flags → env → defaults:

| Flag | Env | Default |
| --- | --- | --- |
| `--addr` | `MEDEA_ADDR` | `localhost:7600` |
| `--token` | `MEDEA_TOKEN` | — |
| `--ca` | `MEDEA_CA` | — (required unless `--insecure`) |

## Typical workflow

```sh
# 1. Seed the store from a live cluster (run with the server STOPPED).
#    Registers the cluster/pools/machines and copies creds into --creds-dir.
medea seed --cluster home \
  --talosconfig ./talosconfig --kubeconfig ./kubeconfig \
  --store /var/lib/medea/medea.db --creds-dir /var/lib/medea/creds

# 2. Start the server (with --rollouts to allow upgrades).

# 3. Read state.
export MEDEA_ADDR=host:7600 MEDEA_TOKEN=… MEDEA_CA=./cert.pem
medea get clusters
medea get nodepools --cluster home
medea get machines  --cluster home

# 4. Arm the cluster (deliberate, separate from seed; default off).
medea cluster enable-rollouts home

# 5. Upgrade. Without --confirm you get a dry-run plan and nothing changes.
medea upgrade --cluster home --pool workers --talos v1.13.6   # plan
medea upgrade --cluster home --pool workers --talos v1.13.6 --confirm
medea upgrade --cluster home --k8s v1.36.2 --confirm          # cluster-wide K8s

# 6. Observe.
medea rollout status --cluster home [--pool workers] [-w]
medea rollout list   --cluster home
```

## Safety model

Accidental action is made **structurally impossible**, not merely unlikely
([`design/rollout-safety.md`](design/rollout-safety.md)):

- **`rolloutsEnabled` per cluster, default off** — never set by seed; checked at
  both job creation *and* execution. A cluster you never enable can never be
  rolled.
- **Manual mode** — editing desired versions is inert; only an explicit,
  confirmed `Rollout` job acts.
- **Plan-then-`--confirm`** — every mutating `upgrade` is a dry run without `--confirm`.
- **Snapshot-before-control-plane** — a control-plane node (or any K8s upgrade)
  is never touched without a fresh etcd snapshot first (the only undo on a
  single-member etcd).
- **Halt-on-failure** — the first node that fails to drain/upgrade/converge
  stops the whole rollout.
- **Resume** — rollout state lives in Medea's store, so a Medea restart or a
  control-plane reboot mid-rollout resumes rather than double-acting or failing.

## Testing

```sh
make test               # unit, race detector (no external deps)
make test-integration   # against a scratch Talos cluster (needs docker + talosctl)
make test-qemu          # faithful OS A/B upgrade on a QEMU cluster (needs qemu + sudo)
make lint               # golangci-lint
make check              # vet + unit (what CI runs)
```

## Documentation map

| Doc | Purpose |
| --- | --- |
| [`PRD.md`](PRD.md) | What/why: scope, goals, non-goals, decisions. |
| [`DOMAIN.md`](DOMAIN.md) | Bounded contexts, ubiquitous language, DDD posture. |
| [`design/`](design/README.md) | Per-subsystem decision records. |
| [`design/aggregates/`](design/aggregates/README.md) | Per-aggregate invariant/lifecycle records. |

## License

Apache-2.0 (see PRD §License).
