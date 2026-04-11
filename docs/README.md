# cloudflare-controller — Cloudflare Argo Tunnel Controller

cloudflare-controller is a Kubernetes controller that watches `Service` objects annotated with `cloudflare-controller.io/hostname` and automatically manages the corresponding Cloudflare resources: a DNS CNAME record, a cloudflared ingress rule in a ConfigMap, and (optionally) a Cloudflare Zero Trust Access Application.

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Project Structure](#project-structure)
3. [Architecture](#architecture)
4. [Service Annotations](#service-annotations)
5. [Reconcile Loop](#reconcile-loop)
6. [Configuration](#configuration)
7. [Build and Test](#build-and-test)
8. [Deploy to a Cluster](#deploy-to-a-cluster)
9. [Run Locally](#run-locally)
10. [Usage Example](#usage-example)
11. [Observing the Controller](#observing-the-controller)

---

## Prerequisites

| Tool | Version | Purpose |
|---|---|---|
| Go | 1.22.5 | Build the manager binary |
| kubectl | 1.27+ | Apply RBAC and run the controller |
| Docker | any | Build the controller image |
| kind / minikube | any | Local cluster for development |
| cloudflared | any | Running Argo Tunnel in-cluster |

The controller targets Kubernetes API version **1.29** (`k8s.io/*` at `v0.29.15`, `controller-runtime` at `v0.17.6`).

A Cloudflare account with an existing **Argo Tunnel** (cloudflared) is required. The controller does not create the tunnel itself — it manages DNS records, ConfigMap ingress rules, and Access Applications on top of an existing tunnel.

---

## Project Structure

```
cloudflare-controller/
├── internal/
│   ├── cloudflare/
│   │   └── client.go          # Cloudflare API client interface + cloudflare-go implementation
│   ├── configmap/
│   │   └── manager.go         # cloudflared ConfigMap ingress upsert/remove with retry-on-conflict
│   ├── config/
│   │   └── config.go          # YAML config loader + validator
│   └── controller/
│       └── service_controller.go  # ServiceReconciler: DNS + ConfigMap + Access App
├── cmd/
│   └── main.go                # Manager bootstrap, config loading, healthz/readyz probes
├── config/
│   ├── rbac/role.yaml         # ClusterRole for the controller
│   └── samples/
│       ├── controller-config.yaml  # Example controller config file
│       └── annotated-service.yaml  # Example annotated Service
├── Dockerfile
├── Makefile
└── go.mod
```

---

## Architecture

```
┌───────────────────────────────────────────────────────────────────┐
│  Kubernetes cluster                                                │
│                                                                   │
│  Service (annotated)  ──watch──►  ServiceReconciler               │
│                                        │                          │
│                         ┌─────────────┼──────────────────┐        │
│                         ▼             ▼                  ▼        │
│                    Cloudflare    cloudflared         Cloudflare   │
│                    DNS CNAME     ConfigMap           Zero Trust   │
│                    record        ingress rule        Access App   │
└───────────────────────────────────────────────────────────────────┘
                              │
                    Cloudflare API
                    (cloudflare-go)
```

### Lifecycle

The controller adds a finalizer (`cloudflare-controller.io/finalizer`) to every annotated Service before making any external changes. On Service deletion or annotation removal, the reconciler cleans up all Cloudflare resources before removing the finalizer.

### ConfigMap management

The cloudflared `ConfigMap` (identified by `--cloudflared-configmap-name` / `--cloudflared-configmap-namespace`) stores a `config.yaml` key in cloudflared's format. The controller upserts or removes ingress rules within that YAML, preserving the mandatory catch-all (`http_status: 404`) as the last rule. Concurrent updates are handled with up to 3 retries on `409 Conflict`.

---

## Service Annotations

### Input annotations (set by you)

| Annotation | Required | Description |
|---|---|---|
| `cloudflare-controller.io/hostname` | Yes | Public hostname to expose (e.g. `app.example.com`). Presence triggers the controller. |
| `cloudflare-controller.io/port` | No | Service port to use as the backend. Defaults to the first port in `spec.ports`. |
| `cloudflare-controller.io/access-enabled` | No | Set to `"true"` to create a Cloudflare Zero Trust Access Application for this hostname. |

### Status annotations (written by the controller)

| Annotation | Description |
|---|---|
| `cloudflare-controller.io/dns-record-id` | Cloudflare DNS record ID for the CNAME (stored for cleanup). |
| `cloudflare-controller.io/access-app-id` | Cloudflare Access Application ID (stored for cleanup). |

---

## Reconcile Loop

```
Reconcile(req)
    │
    ├─ GET Service
    │     └─ NotFound → return (already gone)
    │
    ├─ Deletion or hostname annotation removed?
    │     └─ Has finalizer → cleanup() → remove finalizer → Update
    │
    ├─ Add finalizer if absent → Update → re-enqueue
    │
    ├─ reconcileDNS()
    │     └─ EnsureDNSRecord: list existing CNAMEs → update if drifted, create if absent
    │         Writes cloudflare-controller.io/dns-record-id annotation
    │
    ├─ ConfigMgr.UpsertIngress()
    │     └─ Parse cloudflared config.yaml from ConfigMap → upsert rule → marshal back
    │         Retries up to 3× on 409 Conflict
    │
    ├─ access-enabled == "true"?
    │     └─ reconcileAccessApp()
    │         Already has app-id → skip; else EnsureAccessApp → write annotation
    │
    ├─ Patch Service annotations (MergeFrom — avoids clobbering other controllers)
    │
    └─ RequeueAfter: 5m
```

### Cleanup

```
cleanup()
    ├─ DeleteDNSRecord (if dns-record-id annotation present)
    ├─ RemoveIngress from ConfigMap (if hostname annotation present)
    └─ DeleteAccessApp (if access-app-id annotation present)
```

---

## Configuration

The controller is configured via a YAML file (`--config`) with individual flags that override file values. `CLOUDFLARE_API_TOKEN` must be provided as an environment variable — never in the config file.

### Config file (`config/samples/controller-config.yaml`)

```yaml
cloudflare:
  accountID: "your-cloudflare-account-id"
  zoneID:    "your-cloudflare-zone-id"
  tunnelID:  "your-argo-tunnel-uuid"

cloudflared:
  configMap:
    name:      cloudflared-config
    namespace: cloudflare-system

# Optional — defaults shown
metricsBindAddress:     ":8080"
healthProbeBindAddress: ":8081"
leaderElect:            false
```

### Environment variable

| Variable | Required | Description |
|---|---|---|
| `CLOUDFLARE_API_TOKEN` | Yes | Cloudflare API token. Mount from a Kubernetes Secret. |

### Flags

All flags override their corresponding config file value.

| Flag | Description |
|---|---|
| `--config` | Path to the YAML config file |
| `--cloudflare-account-id` | Cloudflare account ID |
| `--cloudflare-zone-id` | Cloudflare DNS zone ID |
| `--cloudflare-tunnel-id` | Argo Tunnel UUID |
| `--cloudflared-configmap-name` | Name of the cloudflared ConfigMap |
| `--cloudflared-configmap-namespace` | Namespace of the cloudflared ConfigMap |
| `--metrics-bind-address` | Prometheus metrics endpoint (default `:8080`) |
| `--health-probe-bind-address` | Health probe endpoint (default `:8081`) |
| `--leader-elect` | Enable leader election |

---

## Build and Test

```bash
# Format, vet, and compile to bin/manager
make build

# Run tests with race detector + coverage report
make test

# Run a specific test
go test -race -run TestReconcile ./internal/controller/...

# Format only
make fmt

# Vet only
make vet
```

---

## Deploy to a Cluster

### 1. Apply RBAC

```bash
kubectl apply -f config/rbac/role.yaml

kubectl create clusterrolebinding cloudflare-controller-manager-rolebinding \
  --clusterrole=cloudflare-controller-manager-role \
  --serviceaccount=cloudflare-system:cloudflare-controller
```

### 2. Create the API token Secret

```bash
kubectl create secret generic cloudflare-api-token \
  --from-literal=CLOUDFLARE_API_TOKEN=<your-token> \
  -n cloudflare-system
```

### 3. Build and push the image

```bash
make docker-build IMG=your-registry/cloudflare-controller:v0.1.0
docker push your-registry/cloudflare-controller:v0.1.0
```

### 4. Deploy the manager

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cloudflare-controller-manager
  namespace: cloudflare-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: cloudflare-controller-manager
  template:
    metadata:
      labels:
        app: cloudflare-controller-manager
    spec:
      serviceAccountName: cloudflare-controller
      containers:
        - name: manager
          image: your-registry/cloudflare-controller:v0.1.0
          args:
            - --config=/etc/cloudflare-controller/config.yaml
          env:
            - name: CLOUDFLARE_API_TOKEN
              valueFrom:
                secretKeyRef:
                  name: cloudflare-api-token
                  key: CLOUDFLARE_API_TOKEN
          volumeMounts:
            - name: config
              mountPath: /etc/cloudflare-controller
              readOnly: true
          ports:
            - name: metrics
              containerPort: 8080
            - name: health
              containerPort: 8081
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8081
            initialDelaySeconds: 15
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8081
            initialDelaySeconds: 5
      volumes:
        - name: config
          configMap:
            name: cloudflare-controller-config
```

---

## Run Locally

```bash
export CLOUDFLARE_API_TOKEN=<your-token>
make run -- --config=config/samples/controller-config.yaml
```

The manager connects via `$KUBECONFIG` / `~/.kube/config`. Disable leader election (the default) during local development.

---

## Usage Example

Annotate any Service to expose it through the Argo Tunnel:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-app
  namespace: default
  annotations:
    cloudflare-controller.io/hostname: "my-app.example.com"
    cloudflare-controller.io/port: "8080"          # optional; defaults to first port
    cloudflare-controller.io/access-enabled: "true" # optional; creates Access Application
spec:
  selector:
    app: my-app
  ports:
    - port: 8080
      targetPort: 8080
```

The controller will:

1. Create a DNS CNAME record `my-app.example.com → <tunnelID>.cfargotunnel.com`
2. Add an ingress rule to the cloudflared ConfigMap: `my-app.example.com → http://my-app.default.svc.cluster.local:8080`
3. Create a Cloudflare Zero Trust Access Application for `my-app.example.com`

To stop managing: remove the `cloudflare-controller.io/hostname` annotation. The controller will delete the DNS record, remove the ingress rule, delete the Access Application, then remove the finalizer.

---

## Observing the Controller

### Kubernetes Events

Events are emitted on the `Service` object:

| Reason | Type | Trigger |
|---|---|---|
| `DNSRecordCreated` | Normal | DNS CNAME record created or confirmed |
| `AccessAppCreated` | Normal | Cloudflare Access Application created |
| `DNSFailed` | Warning | Cloudflare DNS API error |
| `ConfigMapFailed` | Warning | cloudflared ConfigMap update failed |
| `AccessAppFailed` | Warning | Cloudflare Access API error |

```bash
kubectl describe svc my-app
```

### Metrics

Prometheus metrics on `:8080/metrics` (controller-runtime defaults: reconcile duration, queue depth, error rate).

### Health probes

| Endpoint | Port | Purpose |
|---|---|---|
| `/healthz` | 8081 | Liveness — 200 once the manager is running |
| `/readyz` | 8081 | Readiness — 200 once the manager is ready |
