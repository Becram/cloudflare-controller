# cloudflare-controller — Technical Architecture

## Table of Contents

1. [System Overview](#system-overview)
2. [Component Diagram](#component-diagram)
3. [Data Flow](#data-flow)
4. [Controller Design](#controller-design)
5. [Cloudflare Resource Lifecycle](#cloudflare-resource-lifecycle)
6. [ConfigMap Management](#configmap-management)
7. [Configuration System](#configuration-system)
8. [Secret Handling](#secret-handling)
9. [Error Handling and Retries](#error-handling-and-retries)
10. [Logging](#logging)
11. [RBAC Model](#rbac-model)
12. [Build and Image](#build-and-image)
13. [Dependencies](#dependencies)
14. [Design Decisions](#design-decisions)
15. [Known Limitations](#known-limitations)

---

## System Overview

cloudflare-controller is a Kubernetes controller built on [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) that bridges Kubernetes Services to [Cloudflare Argo Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/). It eliminates the need to manually manage DNS records, tunnel ingress rules, or Cloudflare Zero Trust applications when deploying services behind an Argo Tunnel.

### What cloudflare-controller does

When a Kubernetes Service is annotated with `cloudflare-controller.io/hostname`, cloudflare-controller:

1. Creates a proxied **CNAME DNS record** in Cloudflare DNS: `<hostname> → <tunnelID>.cfargotunnel.com`
2. Upserts an **ingress rule** in the cloudflared ConfigMap: `<hostname> → http://<svc>.<ns>.svc.cluster.local:<port>`
3. Optionally creates a **Cloudflare Zero Trust Access Application** for the hostname

When the annotation is removed or the Service is deleted, cloudflare-controller reverses all three operations in a guaranteed cleanup sequence enforced by a Kubernetes finalizer.

### What cloudflare-controller does NOT do

- Does not create or manage the Argo Tunnel itself (tunnel must pre-exist)
- Does not manage cloudflared deployment or credentials
- Does not configure Access Policies (only creates the Access Application shell)
- Does not support multiple tunnels per controller instance
- Does not create custom CRDs — operates entirely on standard `core/v1` resources

---

## Component Diagram

```
┌─────────────────────────────────────────────────────────────────────────┐
│  Kubernetes Cluster                                                      │
│                                                                          │
│  ┌──────────────────────────────────┐                                   │
│  │  cloudflared namespace           │                                   │
│  │                                  │                                   │
│  │  ┌─────────────────────────┐    │   ┌──────────────────────────┐   │
│  │  │  cloudflared Deployment │◄───┼───│  cloudflared ConfigMap   │   │
│  │  │  (tunnel daemon)        │    │   │  (ingress rules)         │   │
│  │  └─────────────────────────┘    │   │                          │   │
│  │                                  │   │  ingress:               │   │
│  │  ┌─────────────────────────┐    │   │  - hostname: app.ex.com │   │
│  │  │  cloudflare-controller-controller      │────┼──►│    service: http://...  │   │
│  │  │  (this controller)      │    │   │  - service: http_status │   │
│  │  └──────────┬──────────────┘    │   │             :404        │   │
│  │             │                    │   └──────────────────────────┘   │
│  └─────────────┼────────────────────┘                                   │
│                │ watches                                                  │
│                ▼                                                          │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  All Namespaces                                                   │   │
│  │                                                                   │   │
│  │  Service (annotated)                                              │   │
│  │    cloudflare-controller.io/hostname: "app.example.com"              │   │
│  │    cloudflare-controller.io/port: "8080"          (optional)         │   │
│  │    cloudflare-controller.io/access-enabled: "true" (optional)        │   │
│  │    cloudflare-controller.io/dns-record-id: "<id>" (written back)     │   │
│  │    cloudflare-controller.io/access-app-id: "<id>" (written back)     │   │
│  └──────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────┘
                │
                │ Cloudflare API (HTTPS)
                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Cloudflare                                                              │
│                                                                          │
│  DNS Zone                         Zero Trust                            │
│  ┌──────────────────────────┐    ┌──────────────────────────────────┐  │
│  │  CNAME record            │    │  Access Application              │  │
│  │  app.example.com         │    │  app.example.com                 │  │
│  │  → tunnelID.cfargotunnel │    │  (self-hosted, session 24h)      │  │
│  │    .com                  │    │                                  │  │
│  └──────────────────────────┘    └──────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## Data Flow

### Creation flow (annotation added to Service)

```
1. User annotates Service
        │
        ▼
2. API Server notifies cloudflare-controller via watch (predicate filters unannotated Services)
        │
        ▼
3. ServiceReconciler.Reconcile() called
        │
        ├─► GET Service from API Server
        │
        ├─► Finalizer absent?
        │       └─► ADD finalizer → UPDATE Service → return
        │           (triggers re-enqueue via watch event)
        │
        ├─► reconcileDNS()
        │       ├─► ListDNSRecords (Cloudflare API) — CNAME for hostname
        │       ├─► Record exists with correct target → return recordID (no-op)
        │       ├─► Record exists with wrong target → UpdateDNSRecord → return recordID
        │       └─► Record absent → CreateDNSRecord → return recordID
        │           Writes cloudflare-controller.io/dns-record-id annotation
        │
        ├─► ConfigMgr.UpsertIngress()
        │       ├─► GET ConfigMap from API Server
        │       ├─► Parse config.yaml YAML key
        │       ├─► Remove existing rule for hostname (idempotent)
        │       ├─► Insert new rule before catch-all
        │       ├─► Marshal back to YAML
        │       └─► PATCH ConfigMap (retry up to 3× on 409 Conflict)
        │
        ├─► access-enabled == "true"?
        │       └─► reconcileAccessApp()
        │               ├─► ListAccessApplications — match by domain
        │               ├─► App exists → return appID (no-op)
        │               └─► App absent → CreateAccessApplication → return appID
        │                   Writes cloudflare-controller.io/access-app-id annotation
        │
        └─► PATCH Service (MergeFrom — only changed annotations)
            RequeueAfter: 5m
```

### Deletion flow (annotation removed or Service deleted)

```
1. DeletionTimestamp set or hostname annotation removed
        │
        ▼
2. Reconcile detects cleanup condition
        │
        ├─► cleanup()
        │       ├─► DeleteDNSRecord (if dns-record-id annotation present)
        │       │       └─► 404 from Cloudflare → treat as success (idempotent)
        │       ├─► RemoveIngress from ConfigMap
        │       └─► DeleteAccessApp (if access-app-id annotation present)
        │               └─► 404 from Cloudflare → treat as success
        │
        ├─► RemoveFinalizer
        └─► UPDATE Service → Kubernetes GC proceeds
```

---

## Controller Design

### ServiceReconciler struct

```go
type ServiceReconciler struct {
    client.Client                  // Kubernetes API client (embedded)
    Scheme             *runtime.Scheme
    Recorder           record.EventRecorder
    ConfigMgr          *configmap.Manager  // cloudflared ConfigMap manager
    ConfigMapName      string
    ConfigMapNamespace string
    CFClient           cfc.Client          // Cloudflare API client (interface)
    AccountID          string
    ZoneID             string
    TunnelID           string
}
```

All Cloudflare configuration is injected at startup as struct fields — there is no runtime re-reading of credentials or configuration. This means a pod restart is required to pick up config changes.

### Watch predicate

The controller uses a compound predicate to avoid reconciling every Service in the cluster:

```go
predicate.Or(
    hasAnnotationOrFinalizer,          // Service has hostname annotation OR cloudflare-controller finalizer
    predicate.AnnotationChangedPredicate{}, // Any annotation change on the Service
)
```

`hasAnnotationOrFinalizer` returns `true` if:
- `cloudflare-controller.io/hostname` annotation is present, OR
- `cloudflare-controller.io/finalizer` finalizer is present (needed to process deletions)

`AnnotationChangedPredicate` ensures the controller picks up the moment a hostname annotation is added to a Service that previously had none.

### Finalizer pattern

The finalizer `cloudflare-controller.io/finalizer` is added to every Service before any external mutation. This guarantees:

1. Kubernetes will not physically delete the Service object until the finalizer is removed
2. The controller always gets a chance to clean up Cloudflare resources before the Service disappears
3. The cleanup path is triggered both on explicit deletion (`DeletionTimestamp != nil`) and on annotation removal (`hostname == ""`) — allowing cleanup without deletion

The finalizer is added in a dedicated reconcile pass that returns early, ensuring the Service is persisted with the finalizer before any Cloudflare API calls are made.

### Annotation patch strategy

Status annotations (`dns-record-id`, `access-app-id`) are written back using `client.MergeFrom`:

```go
patch := client.MergeFrom(svc.DeepCopy())  // snapshot before mutations
// ... mutate svc.Annotations ...
r.Patch(ctx, svc, patch)                   // only the diff is sent
```

This avoids overwriting annotations managed by other controllers (e.g., Helm, ArgoCD, monitoring scrapers). A full `Update` would send the entire object, potentially clobbering concurrent changes.

### Requeue interval

The controller requeues every **5 minutes** (`requeueAfter = 5 * time.Minute`) to detect and reconcile any drift — for example if a DNS record was manually deleted in Cloudflare or a ConfigMap ingress rule was accidentally overwritten.

---

## Cloudflare Resource Lifecycle

### DNS Record

- **Type**: `CNAME`
- **Name**: `<hostname>` (e.g. `app.example.com`)
- **Content**: `<tunnelID>.cfargotunnel.com`
- **TTL**: `1` (auto / proxied)
- **Proxied**: `true` (traffic goes through Cloudflare proxy)
- **Comment**: `managed by cloudflare-controller`

The `EnsureDNSRecord` function is idempotent:
- Lists existing CNAME records for the hostname
- If a record with the correct target exists → returns its ID, no API write
- If a record exists with a different target → updates it (handles tunnel migration)
- If no record exists → creates it

Record ID is stored in `cloudflare-controller.io/dns-record-id` annotation for targeted deletion. Without the ID, deletion would require a list+filter API call.

### Cloudflare Access Application

- **Type**: `SelfHosted`
- **Domain**: `<hostname>`
- **Session duration**: `24h`

The `EnsureAccessApp` function lists all Access Applications for the account and matches by `Domain`. If one already exists for the hostname, it returns the existing ID without creating a duplicate.

The Access Application is only a shell — it enables Zero Trust protection but has no Access Policies configured by cloudflare-controller. Policies must be added manually in the Cloudflare dashboard or via separate tooling.

### Not-found handling

Both `DeleteDNSRecord` and `DeleteAccessApp` treat Cloudflare 404-equivalent responses as success. This makes cleanup idempotent — if a resource was manually deleted in Cloudflare between reconcile cycles, the controller will not fail or get stuck.

Cloudflare error codes treated as "not found":
- `7003` — Invalid resource identifier (record/app doesn't exist)
- `1001` — Resource not found
- `81044` — DNS record not found

---

## ConfigMap Management

The cloudflared daemon reads its configuration from a `config.yaml` key in a Kubernetes ConfigMap. cloudflare-controller directly mutates this ConfigMap to add and remove ingress rules.

### ConfigMap structure

```yaml
tunnel: <tunnel-name>
credentials-file: /etc/cloudflared/creds/credentials.json
ingress:
  - hostname: app1.example.com
    service: http://app1.default.svc.cluster.local:8080
  - hostname: app2.example.com
    service: http://app2.apps.svc.cluster.local:3000
  - service: http_status:404    # catch-all — always last
```

### Invariants maintained by cloudflare-controller

1. **Catch-all rule is always last**: cloudflared requires a catch-all entry as the final ingress rule. cloudflare-controller enforces this on every write by calling `insertBeforeCatchAll`.
2. **No duplicate rules**: before inserting a rule, `removeByHostname` removes any existing rule for that hostname. This makes `UpsertIngress` idempotent.
3. **Catch-all is never removed**: `removeByHostname` matches on `Hostname` field — the catch-all has an empty `Hostname`, so it is never removed by hostname-based operations.

### Optimistic locking retry

The ConfigMap is a shared resource — cloudflared may update it and other tooling may write to it concurrently. Kubernetes uses `resourceVersion` for optimistic locking: a `PATCH` or `UPDATE` that sends a stale `resourceVersion` returns `409 Conflict`.

`retryOnConflict` handles this by re-fetching the ConfigMap on each attempt:

```
attempt 0: GET ConfigMap (rv=100) → mutate → PATCH → success
attempt 0: GET ConfigMap (rv=100) → mutate → PATCH → 409 (rv now 101)
attempt 1: GET ConfigMap (rv=101) → mutate → PATCH → success
```

Maximum 3 attempts. If all 3 fail (highly contended ConfigMap), the reconcile returns an error and controller-runtime requeues with exponential backoff.

### cloudflared hot reload

cloudflared watches its mounted ConfigMap volume and reloads configuration when the file changes. Since Kubernetes propagates ConfigMap updates to mounted volumes within ~1 minute (kubelet sync period), ingress rule changes are picked up by cloudflared without a restart.

---

## Configuration System

### Precedence (highest to lowest)

```
CLI flags  >  config file  >  built-in defaults
```

| Source | Example |
|---|---|
| CLI flag | `--cloudflare-account-id=abc123` |
| Config file | `cloudflare.accountID: abc123` in YAML |
| Default | `metricsBindAddress: ":8080"` |

### Config struct

```go
type Config struct {
    Cloudflare  CloudflareConfig  // accountID, zoneID, tunnelID
    Cloudflared CloudflaredConfig // configMap.name, configMap.namespace
    MetricsBindAddress     string // default: ":8080"
    HealthProbeBindAddress string // default: ":8081"
    LeaderElect            bool   // default: false
    LogLevel               string // default: "info"
}
```

### Validation

`Config.Validate()` is called after all sources are merged. It fails fast if any required field is missing:
- `cloudflare.accountID`
- `cloudflare.zoneID`
- `cloudflare.tunnelID`
- `cloudflared.configMap.name`
- `cloudflared.configMap.namespace`

All missing fields are collected and returned in a single error, rather than failing on the first missing field.

---

## Secret Handling

### CLOUDFLARE_API_TOKEN

The Cloudflare API token is **never** read from the config file. It must be provided as the `CLOUDFLARE_API_TOKEN` environment variable. This separation:

- Prevents accidental token exposure in ConfigMaps or GitOps repositories
- Allows the token to be rotated independently of the controller config
- Follows the twelve-factor app principle of strict config/secret separation

In production (Kubernetes), the token is mounted from a Kubernetes Secret:

```yaml
env:
  - name: CLOUDFLARE_API_TOKEN
    valueFrom:
      secretKeyRef:
        name: cloudflare-controller-config
        key: CLOUDFLARE_API_TOKEN
```

The Secret itself is managed by [external-secrets](https://external-secrets.io) pulling from AWS SSM Parameter Store — the plaintext token never appears in any Git repository or Kubernetes manifest.

### Secret lifecycle

The token is read once at startup from `os.Getenv("CLOUDFLARE_API_TOKEN")`. If the variable is empty, the process exits immediately with a non-zero code. The token is passed directly to `cf.NewWithAPIToken()` and held in memory for the lifetime of the process — it is never logged, written to disk, or included in any Kubernetes object.

In Kubernetes, the token should be mounted from a Secret as an environment variable:

```yaml
env:
  - name: CLOUDFLARE_API_TOKEN
    valueFrom:
      secretKeyRef:
        name: <secret-name>
        key: CLOUDFLARE_API_TOKEN
```

---

## Error Handling and Retries

### Reconcile errors

Any error returned from `Reconcile()` causes controller-runtime to requeue the item with **exponential backoff** (starting at ~1s, capped at ~1000s by default). This handles transient Cloudflare API failures, network timeouts, and Kubernetes API errors.

### Partial failure

If DNS reconciliation succeeds but ConfigMap update fails, the DNS record ID annotation has already been written (via the patch). On the next reconcile, `reconcileDNS` will find the existing record, return its ID (no-op), and the ConfigMap update will be retried. This ensures eventual consistency without duplicate resource creation.

### Kubernetes events

The controller emits `Warning` events on the Service object for every error path:

| Reason | Trigger |
|---|---|
| `DNSFailed` | Cloudflare DNS API error |
| `ConfigMapFailed` | cloudflared ConfigMap patch failed |
| `AccessAppFailed` | Cloudflare Access API error |

And `Normal` events for successful actions:

| Reason | Trigger |
|---|---|
| `DNSRecordCreated` | DNS CNAME record created or confirmed |
| `AccessAppCreated` | Access Application created |

Events are observable via `kubectl describe svc <name>` and are stored in Kubernetes for ~1 hour.

---

## Logging

### Log levels

| Level | Behaviour |
|---|---|
| `debug` | All V(1) debug logs enabled, development mode (human-readable, stack traces) |
| `info` | Operational logs only (reconcile milestones, resource creation/deletion) |
| `warn` | Warnings only |
| `error` | Errors only |

### Log initialisation order

The logger is initialised **twice**:

1. **Bootstrap** — immediately after flag parsing, using only `--zap-*` flag values. Used to log config file load errors.
2. **Final** — after config file and all flags are resolved. Sets the correct level and enables/disables development mode based on `logLevel`.

This two-phase approach ensures that `--zap-log-level` (zap flag) takes precedence over `logLevel` (config file), while also allowing the config file to set the level when no zap flag is provided.

### Structured log fields

All reconcile log lines include controller-runtime's standard context fields:
- `controller` — always `"service"`
- `namespace` / `name` — the Service being reconciled
- `reconcileID` — unique UUID per reconcile invocation (useful for tracing)

Additional fields added by cloudflare-controller:
- `hostname` — the Cloudflare hostname being managed
- `backendURL` — the in-cluster service URL
- `recordID` / `appID` — Cloudflare resource identifiers
- `attempt` — retry attempt number (ConfigMap conflicts)

---

## RBAC Model

cloudflare-controller uses a `ClusterRole` because it watches Services across all namespaces and manages a ConfigMap in a specific namespace (cloudflared).

### Permissions required

| API Group | Resource | Verbs | Reason |
|---|---|---|---|
| `""` (core) | `services` | `get, list, watch, update, patch` | Watch annotated Services; write finalizer and status annotations |
| `""` (core) | `services/finalizers` | `update` | Add/remove the cleanup finalizer |
| `""` (core) | `configmaps` | `get, list, watch, update, patch` | Read and patch cloudflared config.yaml |
| `""` (core) | `events` | `create, patch` | Emit reconcile events on Service objects |
| `coordination.k8s.io` | `leases` | `get, list, watch, create, update, patch, delete` | Leader election (required when `leaderElect: true`) |

### ServiceAccount binding

The `ClusterRoleBinding` maps the `ClusterRole` to the `cloudflare-controller-controller` ServiceAccount in the `cloudflared` namespace. The binding is cluster-scoped (ClusterRoleBinding, not RoleBinding) because the watch covers all namespaces.

---

## Build and Image

### Multi-stage Dockerfile

```
Stage 1 — builder (golang:1.22.5-alpine)
  ├─ Copy go.mod / go.sum
  ├─ go mod download  (cached via BuildKit --mount=type=cache)
  ├─ Copy source
  └─ go build -ldflags="-w -s" → /workspace/manager
        CGO_ENABLED=0, GOOS=linux (static binary, no libc dependency)

Stage 2 — runtime (gcr.io/distroless/static:nonroot)
  ├─ COPY manager binary from builder
  ├─ USER 65532:65532 (nonroot)
  └─ ENTRYPOINT ["/manager"]
```

**Distroless** provides no shell, no package manager, and no OS utilities — the attack surface is limited to the Go binary itself. The `nonroot` variant runs as UID 65532 by default.

### Build optimisations

- **BuildKit module cache**: `--mount=type=cache,target=/root/.cache/go-build` and `/go/pkg/mod` — Go build cache and module cache are reused across builds, avoiding re-downloading and recompiling unchanged dependencies
- **GHA layer cache**: `cache-from: type=gha` / `cache-to: type=gha,mode=max` — Docker layer cache is persisted between GitHub Actions runs
- **Static binary**: `CGO_ENABLED=0` produces a fully static binary with no libc dependency, compatible with the distroless base

### Image tags (published on GitHub Release)

| Tag pattern | Example | Use |
|---|---|---|
| `{major}.{minor}.{patch}` | `1.2.3` | Exact version pin |
| `{major}.{minor}` | `1.2` | Minor version tracking |
| `sha-{short-sha}` | `sha-abc1234` | Commit-level traceability |

---

## Dependencies

### Go modules

| Module | Version | Purpose |
|---|---|---|
| `sigs.k8s.io/controller-runtime` | `v0.17.6` | Controller framework, manager, reconciler base |
| `k8s.io/client-go` | `v0.29.15` | Kubernetes API client, event recorder |
| `k8s.io/apimachinery` | `v0.29.15` | API types, errors, runtime |
| `k8s.io/api` | `v0.29.15` | Core API types (Service, ConfigMap) |
| `github.com/cloudflare/cloudflare-go` | `v0.115.0` | Cloudflare API client |
| `gopkg.in/yaml.v3` | `v3.0.1` | cloudflared config.yaml parsing |
| `go.uber.org/zap` | (transitive) | Structured logging via controller-runtime/zap |

### External systems

| System | How used |
|---|---|
| Cloudflare DNS API | Create/update/delete CNAME records |
| Cloudflare Zero Trust API | Create/delete Access Applications |
| Kubernetes API Server | Watch Services, patch ConfigMap, write events |
| Kubernetes API Server | Watch Services, patch ConfigMap, write events |

---

## Design Decisions

### No custom CRDs

The original design used a `CloudflareTunnel` CRD. This was abandoned in favour of Service annotations because:

- **Zero installation overhead**: no `kubectl apply -f crd.yaml` before deploying, no CRD version management
- **Works with existing Services**: operators can annotate Services they already own without creating additional objects
- **Simpler RBAC**: no additional API group permissions required
- **Lower cognitive overhead**: annotations are visible directly on the resource they affect

The trade-off is that annotation-based configuration is less structured than a CRD spec (no schema validation, no status subresource). This is acceptable for a small number of configuration fields.

### Single tunnel per controller

The controller manages one Argo Tunnel (configured at startup). A multi-tenant design would require:
- Either a label/annotation to select which tunnel to use per Service
- Or multiple controller instances with different configurations

Single-tunnel was chosen because the primary use case is a homelab/small cluster with one tunnel. The design can be extended to multi-tunnel by adding a `tunnelID` override annotation.

### ConfigMap as ingress source of truth

cloudflare-controller writes ingress rules directly to the cloudflared ConfigMap rather than managing a separate Ingress object. This keeps cloudflared's configuration format intact and avoids adding a translation layer. The downside is that cloudflare-controller must parse and maintain cloudflared's YAML format, which is tightly coupled to cloudflared's config schema.

### client.MergeFrom for annotation writes

Using `client.MergeFrom` (strategic merge patch) instead of `Update` ensures cloudflare-controller only writes the fields it owns. A full `Update` would send the entire object, potentially overwriting concurrent changes by other controllers (e.g., ArgoCD reconciling Service labels, Prometheus adding scrape annotations). The merge patch sends only the changed fields.

### Idempotent Cloudflare operations

All Cloudflare API operations are designed to be idempotent:
- `EnsureDNSRecord` checks for existing records before creating
- `EnsureAccessApp` lists existing apps before creating
- Delete operations treat 404 as success

This allows the reconcile loop to run at any frequency without accumulating duplicate resources in Cloudflare.

---

## Known Limitations

### CloudFlared ConfigMap format coupling

cloudflare-controller parses cloudflared's `config.yaml` format. If cloudflared changes its configuration schema in a future version, the `cloudflaredConfig` struct in `internal/configmap/manager.go` may need to be updated. The `Tunnel` and `CredentialsFile` fields are preserved through round-trip marshal/unmarshal but never modified.

### No Access Policy management

cloudflare-controller creates Cloudflare Access Applications but does not create Access Policies. A freshly created Access Application allows no traffic by default — policies must be configured manually. This is intentional (policy configuration is complex and organisation-specific) but may surprise users expecting full Zero Trust setup from the annotation alone.

### ConfigMap must pre-exist

cloudflare-controller does not create the cloudflared ConfigMap — it only patches an existing one. If the ConfigMap does not exist, `UpsertIngress` returns an error and the reconcile fails. The ConfigMap must be created by cloudflared's initial deployment before cloudflare-controller can manage it.

### Single controller instance (no HA by default)

`leaderElect: false` is the default. Running multiple replicas without leader election causes split-brain — both instances will patch the ConfigMap simultaneously, causing high 409 conflict rates. Enable `leaderElect: true` when running more than one replica.

### Annotation removal does not immediately clean up

The cleanup path runs on the next reconcile after annotation removal. If the controller is down when an annotation is removed, cleanup will run on the next startup reconcile. The finalizer prevents the Service from being deleted until cleanup completes, but DNS records and ConfigMap entries remain live until the controller is back up.

### No pagination on Cloudflare API list calls

`EnsureDNSRecord` and `EnsureAccessApp` use un-paginated list calls. For zones with thousands of DNS records or accounts with hundreds of Access Applications, the first page may not contain the target record, resulting in false "not found" — and a duplicate resource being created. In practice this is not an issue for small to medium deployments.
