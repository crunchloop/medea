# MCP delivery adapter

**Status:** Implemented (read-only surface); task-backed operations deferred
**Date:** 2026-07-15

Scope: the Model Context Protocol adapter Iris uses to inspect Medea, its
transport and security boundary, and how long-running Medea operations should
map to MCP Tasks once the Go ecosystem supports the extension.

## 1. Shape

MCP is a peer delivery adapter beside gRPC. It does not read bbolt directly and
does not define another application service:

```text
Iris ── Streamable HTTP ──> internal/delivery/mcpapi
                                      │
                                      v
                            internal/server.Server
                                      │
                                      v
                                  store.Store
```

`internal/server.Server` satisfies the small `mcpapi.Queries` interface. This
keeps all validation and read semantics on the same path as the CLI and other
gRPC clients. The official MCP Go SDK is confined to `internal/delivery/mcpapi`.

This follows Ariadne's delivery posture: typed tool arguments, a curated tool
suite, structured results, compact text fallbacks, stable structured errors,
and adapter-level contract tests.

## 2. Curated tool surface

The first surface is intentionally read-only:

| Tool | Medea operation |
| --- | --- |
| `list_clusters` | `ListClusters` |
| `get_cluster` | `GetCluster` |
| `list_node_pools` | `ListNodePools` |
| `list_machines` | `ListMachines` |
| `list_hosts` | `ListHosts` |
| `get_rollout` | `GetRollout` |
| `list_rollouts` | `ListRollouts` |

Every tool advertises `readOnlyHint`, `idempotentHint`, and a closed-world
`openWorldHint`. Protobuf responses are encoded with protobuf JSON semantics,
snake_case field names, and symbolic enum values. `structuredContent` is the
lossless result; text content only identifies the result and reports counts.

Credential retrieval is not exposed. Neither are cluster creation/deletion,
host mutations, desired-version writes, safety-gate changes, or rollout
controls. Raw store access and arbitrary query tools are also out of scope.

## 3. Transport and security

`medea serve --mcp-listen <address>` enables Streamable HTTP at `/mcp`; the
empty default disables it. The endpoint has no application-layer
authentication in this first version because Iris's current remote MCP client
supports anonymous or OAuth connections, while Medea's existing API uses a
static gRPC bearer token.

Consequences:

- the MCP endpoint is read-only and cannot return stored credentials;
- bind it to loopback, a private sidecar network, or another trusted interface;
- for a shared network, put it behind an authenticated TLS reverse proxy and
  network policy;
- do not add mutations until Medea and Iris share an authenticated MCP
  authorization context. Iris approval is a human policy gate, not network
  authentication and not a replacement for Medea's rollout safety chain.

MCP and gRPC use separate listeners because gRPC is TLS-only today and the MCP
Go SDK owns an HTTP handler. Converging them behind one reverse proxy is a
deployment concern, not an adapter concern.

## 4. Errors

Domain/application failures are tool execution errors so a model can inspect
and correct its arguments. They use stable codes:

| gRPC status | MCP problem code |
| --- | --- |
| `InvalidArgument` | `invalid_request` |
| `NotFound` | `not_found` |
| `FailedPrecondition` | `failed_precondition` |
| `Aborted` | `conflict` |
| `Canceled`, `DeadlineExceeded` | `query_canceled` |
| other | `query_unavailable` (detail redacted) |

## 5. MCP Tasks and Medea operations

Tasks are useful, but not for the read tools above. The strong fit is an
eventual `create_rollout` MCP tool:

1. The synchronous Medea command validates the full safety chain and durably
   creates the rollout job.
2. When Iris and Medea negotiate the Tasks extension, the MCP call returns that
   durable operation as a task handle.
3. `tasks/get` projects the persisted rollout job; store watch events can later
   drive task-status notifications without changing the source of truth.
4. A non-Tasks client receives the normal immediate "job accepted" result.

The current Tasks extension is [SEP-2663](https://modelcontextprotocol.io/seps/2663-tasks-extension),
which supersedes the experimental core Tasks API from MCP 2025-11-25. It uses
extension negotiation, server-directed task creation, `tasks/get`,
`tasks/update`, and `tasks/cancel`; terminal results are returned inline by
`tasks/get`. As of this record, Tasks remain on the official Go SDK roadmap, so
Medea does not implement a private copy of the wire protocol.

The future rollout projection is:

| Medea rollout job | MCP task |
| --- | --- |
| `PENDING`, `RUNNING` | `working` |
| `PAUSED` | `working`, with a paused status message |
| `DONE` | `completed`, with the final rollout result |
| `FAILED` | `completed`, with `CallToolResult.isError=true` (an application outcome, not a JSON-RPC failure) |
| `CANCELLED` | `cancelled` |

`PAUSED` must not become `input_required` unless Medea actually emits a bounded
request that `tasks/update` can satisfy. A separate operator may resume a
rollout; merely being paused is not necessarily a question for the task caller.

### Persistence prerequisite

Medea currently keys one rollout job by `(cluster, pool)` and overwrites it on a
later rollout. That is enough for the v1 executor but not for MCP's durable task
retention: an old task ID must continue resolving for its TTL after a newer job
starts. Before enabling Tasks, the Rollout aggregate needs a stable job ID and
retained terminal history (or an equivalently durable projection). A second,
MCP-only task database is rejected because it would duplicate lifecycle truth.

Cluster bootstrap is another potential task-backed tool, but it first needs a
read application operation for bootstrap status and the same durable identity
analysis. It is not exposed by reaching around the application boundary into
the store.

## 6. Decisions

1. MCP is a delivery adapter over `internal/server`, not a store client.
2. The initial catalog is curated and read-only; credentials and mutations are
   absent by construction.
3. Streamable HTTP is used because it is the transport Iris consumes.
4. Structured protobuf JSON is canonical; text is a compact fallback.
5. Rollouts should use SEP-2663 Tasks when client and SDK support lands, backed
   by the Rollout aggregate after it gains durable identity/history.
6. No proprietary Tasks wire shim and no duplicate task store.
