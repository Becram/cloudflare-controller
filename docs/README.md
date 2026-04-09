# Rector — Kubernetes Application Controller

Rector is a Kubernetes controller that introduces an `Application` Custom Resource Definition (CRD). Each `Application` CR represents a containerised workload: the controller reconciles it into a `Deployment` and a `Service` in the same namespace, keeping them in sync with the desired spec and reporting availability back through status conditions.

---

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Project Structure](#project-structure)
3. [Architecture](#architecture)
4. [CRD Spec Reference](#crd-spec-reference)
5. [Reconcile Loop](#reconcile-loop)
6. [Status and Conditions](#status-and-conditions)
7. [Build and Test](#build-and-test)
8. [Deploy to a Cluster](#deploy-to-a-cluster)
9. [Run Locally](#run-locally)
10. [Creating an Application CR](#creating-an-application-cr)
11. [Observing the Controller](#observing-the-controller)
12. [Code Generation](#code-generation)
13. [Manager Flags](#manager-flags)

---

## Prerequisites

| Tool | Version | Purpose |
|---|---|---|
| Go | 1.22.5 | Build the manager binary |
| kubectl | 1.27+ | Apply CRDs and sample CRs |
| controller-gen | v0.14.0 | Regenerate CRD manifests and deepcopy (invoked via `go run`, no global install needed) |
| Docker | any | Build the controller image |
| kind / minikube | any | Local cluster for development |

The controller targets Kubernetes API version **1.29** (`k8s.io/*` at `v0.29.15`, `controller-runtime` at `v0.17.6`).

---

## Project Structure

```
rector/
├── api/v1alpha1/
│   ├── application_types.go        # ApplicationSpec, ApplicationStatus, kubebuilder markers
│   ├── groupversion_info.go        # Group: apps.rector.io, Version: v1alpha1
│   └── zz_generated.deepcopy.go   # Generated — do not edit
├── internal/controller/
│   └── application_controller.go  # Reconciler: Deployment + Service + status
├── cmd/
│   └── main.go                    # Manager bootstrap, scheme registration, probes
├── config/
│   ├── crd/                       # CRD manifest (regenerate with `make manifests`)
│   ├── rbac/role.yaml             # ClusterRole (regenerate with `make manifests`)
│   └── samples/                   # Example Application CR
├── hack/
│   └── boilerplate.go.txt         # License header for generated files
├── Dockerfile                     # Multi-stage build → distroless image
├── Makefile
└── go.mod
```

---

## Architecture

### Overview

```
┌─────────────────────────────────────────────────────────┐
│  Kubernetes API Server                                   │
│                                                          │
│  Application CR  ──owns──►  Deployment                  │
│  (apps.rector.io)  ──owns──►  Service                   │
└─────────────────────────────────────────────────────────┘
            ▲                        │
            │  watch / reconcile     │ CreateOrUpdate
            │                        ▼
┌───────────────────────────────────────┐
│  rector-controller (manager process)  │
│                                       │
│  ApplicationReconciler                │
│    ├── reconcileDeployment()          │
│    ├── reconcileService()             │
│    └── updateConditions()             │
└───────────────────────────────────────┘
```

### Ownership model

The controller sets an **owner reference** (`controller: true`) on both the `Deployment` and the `Service`, pointing back to the `Application` CR. This means:

- Deleting an `Application` CR cascades to delete its `Deployment` and `Service` automatically via Kubernetes garbage collection — no finalizers are needed.
- Both owned resources must live in the **same namespace** as the `Application` (owner references do not cross namespaces).

### Watch chain

`SetupWithManager` registers three watches:

```go
ctrl.NewControllerManagedBy(mgr).
    For(&appsv1alpha1.Application{}).      // primary watch
    Owns(&appsv1.Deployment{}).            // re-enqueue parent on child change
    Owns(&corev1.Service{}).               // re-enqueue parent on child change
    Complete(r)
```

Any change to an owned `Deployment` or `Service` — whether made by the user, another controller, or a rollout — re-enqueues the parent `Application` for reconciliation. This is how the controller self-heals manual edits to owned resources.

### Labels

Every owned resource receives these labels, which also serve as the pod selector:

```
app.kubernetes.io/name:       <application-name>
app.kubernetes.io/managed-by: rector-controller
```

---

## CRD Spec Reference

**Group:** `apps.rector.io`  
**Version:** `v1alpha1`  
**Kind:** `Application`  
**Scope:** Namespaced  
**Short name:** `app`  
**Categories:** `all` (appears in `kubectl get all`)

### `spec` fields

| Field | Type | Required | Default | Constraints | Description |
|---|---|---|---|---|---|
| `image` | `string` | Yes | — | — | Container image to deploy (e.g. `nginx:1.25-alpine`) |
| `port` | `int32` | Yes | — | 1–65535 | Container port to expose; Service `port` and `targetPort` are both set to this value |
| `replicas` | `*int32` | No | `1` | ≥ 0 | Desired pod count. Set to `0` to scale down without deleting the Application |
| `serviceType` | `string` | No | `ClusterIP` | `ClusterIP`, `NodePort`, `LoadBalancer` | Kubernetes Service type |
| `env` | `[]corev1.EnvVar` | No | — | — | Environment variables; supports `value`, `valueFrom.secretKeyRef`, `valueFrom.configMapKeyRef`, `valueFrom.fieldRef` |
| `resources` | `corev1.ResourceRequirements` | No | — | — | Container resource `requests` and `limits` |
| `serviceAnnotations` | `map[string]string` | No | — | — | Annotations applied to the managed `Service` object (e.g. Prometheus scrape hints, internal load-balancer flags) |
| `tolerations` | `[]corev1.Toleration` | No | — | — | Tolerations added to the pod spec; allows pods to be scheduled on nodes carrying matching taints |

### `status` fields

| Field | Type | Description |
|---|---|---|
| `availableReplicas` | `int32` | Number of pods with a `Ready` condition, sourced from `Deployment.Status.AvailableReplicas` |
| `conditions` | `[]metav1.Condition` | Standard Kubernetes condition list — see [Status and Conditions](#status-and-conditions) |

---

## Reconcile Loop

The reconciler runs every time an `Application` CR changes, or every **30 seconds** as a background requeue (to catch drift from Deployment status updates).

```
Reconcile(req)
    │
    ├─ GET Application CR
    │     └─ NotFound → return (object deleted, GC handles owned resources)
    │
    ├─ Capture status patch base (MergeFrom snapshot before any mutations)
    │
    ├─ reconcileDeployment()
    │     ├─ CreateOrUpdate Deployment with name = app.Name, namespace = app.Namespace
    │     │     Spec: replicas, selector, pod template (image, port, env, resources, tolerations)
    │     ├─ SetControllerReference → owner reference to Application
    │     └─ Record Event on create or update
    │
    ├─ reconcileService()
    │     ├─ CreateOrUpdate Service with name = app.Name, namespace = app.Namespace
    │     │     Annotations: app.Spec.ServiceAnnotations (fully replaced on each reconcile)
    │     │     Spec: type, selector, port = targetPort = app.Spec.Port
    │     │     ⚠ Preserves existing Spec.ClusterIP (immutable after creation)
    │     ├─ SetControllerReference → owner reference to Application
    │     └─ Record Event on create or update
    │
    ├─ app.Status.AvailableReplicas = deploy.Status.AvailableReplicas
    ├─ updateConditions()
    │
    ├─ defer: r.Status().Patch() → write status diff back to API server
    │         (uses Status subresource — r.Update() on the main object will NOT persist status)
    │
    └─ return RequeueAfter: 30s
```

### Why `CreateOrUpdate` and not `Apply`

`controllerutil.CreateOrUpdate` fetches the current object, runs the mutate function to set desired fields, then issues a Create or Update call. It is idempotent and avoids field manager conflicts. The mutate function only sets fields the controller owns — it does not overwrite fields set by other controllers (e.g. `Spec.ClusterIP`, admission webhook annotations).

---

## Status and Conditions

The controller writes two conditions to `status.conditions` using `k8s.io/apimachinery/pkg/api/meta.SetStatusCondition`, which enforces the standard condition list contract (map keyed by `type`, `lastTransitionTime` only updated on actual state change).

### `Available`

| State | Status | Reason | Message |
|---|---|---|---|
| `availableReplicas >= desiredReplicas` | `True` | `MinimumReplicasAvailable` | `N/N replicas available` |
| `availableReplicas < desiredReplicas` | `False` | `MinimumReplicasUnavailable` | `N/M replicas available` |

### `Progressing`

| State | Status | Reason | Message |
|---|---|---|---|
| Rollout complete | `False` | `NewReplicaSetAvailable` | `Deployment has successfully rolled out` |
| Rollout in progress | `True` | `ReplicaSetUpdated` | `Deployment is progressing` |

### Reading conditions

```bash
kubectl get app my-app -o jsonpath='{.status.conditions}' | jq .
```

Example output:

```json
[
  {
    "type": "Available",
    "status": "True",
    "reason": "MinimumReplicasAvailable",
    "message": "2/2 replicas available",
    "lastTransitionTime": "2026-04-09T08:00:00Z",
    "observedGeneration": 1
  },
  {
    "type": "Progressing",
    "status": "False",
    "reason": "NewReplicaSetAvailable",
    "message": "Deployment has successfully rolled out",
    "lastTransitionTime": "2026-04-09T08:00:00Z",
    "observedGeneration": 1
  }
]
```

`observedGeneration` matches `metadata.generation` when the status reflects the current spec. A mismatch means the controller has not yet reconciled the latest change.

---

## Build and Test

```bash
# Format, vet, and compile to bin/manager
make build

# Run tests with race detector + coverage report
make test

# Run only a specific test
go test -race -run TestApplicationReconciler ./internal/controller/...

# Format only
make fmt

# Vet only
make vet
```

`make build` runs `generate`, `fmt`, and `vet` as prerequisites. Do not bypass them in CI.

---

## Deploy to a Cluster

### 1. Install the CRD

```bash
make install
# equivalent to: kubectl apply -f config/crd/
```

Verify:

```bash
kubectl get crd applications.apps.rector.io
```

### 2. Apply RBAC

```bash
kubectl apply -f config/rbac/role.yaml

# Create the ClusterRoleBinding (adjust serviceaccount name/namespace as needed)
kubectl create clusterrolebinding rector-manager-rolebinding \
  --clusterrole=rector-manager-role \
  --serviceaccount=default:rector-controller
```

### 3. Build and push the image

```bash
make docker-build IMG=your-registry/rector-controller:v0.1.0
docker push your-registry/rector-controller:v0.1.0
```

### 4. Deploy the manager

Create a `Deployment` in your cluster that runs the manager image with appropriate ServiceAccount, resource limits, and the flags listed in [Manager Flags](#manager-flags). A minimal example:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: rector-controller-manager
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: rector-controller-manager
  template:
    metadata:
      labels:
        app: rector-controller-manager
    spec:
      serviceAccountName: rector-controller
      containers:
        - name: manager
          image: your-registry/rector-controller:v0.1.0
          args:
            - --leader-elect=true
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
```

### Remove the CRD

```bash
make uninstall
# This deletes all Application CRs as well — all owned Deployments and Services will be GC'd.
```

---

## Run Locally

Runs the controller process on your machine against whatever cluster `kubectl` is currently pointing to. The CRD must already be installed.

```bash
make install   # install CRD if not already present
make run       # go run ./cmd/main.go
```

The manager connects to the cluster via `$KUBECONFIG` or `~/.kube/config`. It does **not** run in-cluster, so leader election should be left disabled (the default) during local development.

---

## Creating an Application CR

### Minimal

```yaml
apiVersion: apps.rector.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: default
spec:
  image: nginx:1.25-alpine
  port: 80
```

This creates:
- A `Deployment` named `my-app` with 1 replica (default), running `nginx:1.25-alpine` on port 80.
- A `ClusterIP` `Service` named `my-app` exposing port 80.

### Full example

```yaml
apiVersion: apps.rector.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: default
spec:
  image: nginx:1.25-alpine
  replicas: 2
  port: 80
  serviceType: ClusterIP
  serviceAnnotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "80"
  env:
    - name: ENV
      value: production
    - name: DB_PASSWORD
      valueFrom:
        secretKeyRef:
          name: my-app-secrets
          key: db-password
  resources:
    requests:
      cpu: 100m
      memory: 64Mi
    limits:
      cpu: 500m
      memory: 128Mi
  tolerations:
    - key: dedicated
      operator: Equal
      value: my-app
      effect: NoSchedule
```

### Tolerations reference

`tolerations[].operator` has two valid values:

| Operator | Behaviour |
|---|---|
| `Equal` | Tolerate taints where `key`, `value`, and `effect` all match. `value` must be set. |
| `Exists` | Tolerate any taint with a matching `key`, regardless of value. Omit `value`. Set `key: ""` with `Exists` to tolerate all taints on a node. |

`tolerations[].effect` filters which taint effects are tolerated:

| Effect | Description |
|---|---|
| `NoSchedule` | Pod will not be scheduled on the node unless it has a matching toleration |
| `PreferNoSchedule` | Scheduler avoids placing the pod on the node but will if no alternatives exist |
| `NoExecute` | Pod is evicted if already running; new pods are not scheduled. Set `tolerationSeconds` to evict after a delay |

**Example — tolerate all taints on a node (use with caution):**
```yaml
tolerations:
  - operator: Exists
```

**Example — stay on a tainted node for 60 s before eviction:**
```yaml
tolerations:
  - key: node.kubernetes.io/not-ready
    operator: Exists
    effect: NoExecute
    tolerationSeconds: 60
```

### Apply and verify

```bash
kubectl apply -f config/samples/apps_v1alpha1_application.yaml

# Short name "app" works because of the `categories=all` and `shortName=app` markers
kubectl get app
# NAME     IMAGE               REPLICAS   AVAILABLE   AGE
# my-app   nginx:1.25-alpine   2          2           30s

# Inspect owned resources
kubectl get deployment,svc -l app.kubernetes.io/name=my-app

# Watch conditions
kubectl get app my-app -o jsonpath='{.status.conditions}' | jq .

# Events from the controller
kubectl describe app my-app | grep -A 20 Events
```

### Scale

```bash
kubectl patch app my-app --type=merge -p '{"spec":{"replicas":5}}'
```

### Scale to zero

```bash
kubectl patch app my-app --type=merge -p '{"spec":{"replicas":0}}'
# Deployment scales to 0; Service remains. Available condition → False.
```

### Delete

```bash
kubectl delete app my-app
# Deployment and Service are GC'd automatically via owner references.
```

---

## Observing the Controller

### Kubernetes Events

The controller emits events on the `Application` object for every reconcile action:

| Reason | Type | Trigger |
|---|---|---|
| `DeploymentReconciled` | Normal | Deployment created or updated |
| `ServiceReconciled` | Normal | Service created or updated |
| `DeploymentFailed` | Warning | Error reconciling Deployment |
| `ServiceFailed` | Warning | Error reconciling Service |

```bash
kubectl describe app my-app
```

### Metrics

The manager exposes Prometheus metrics on `:8080/metrics` (controller-runtime default metrics: reconcile duration, queue depth, errors).

### Health probes

| Endpoint | Port | Purpose |
|---|---|---|
| `/healthz` | 8081 | Liveness — always returns 200 once the manager is running |
| `/readyz` | 8081 | Readiness — returns 200 once the manager is ready to serve |

---

## Code Generation

Re-run after any change to types or `+kubebuilder:` markers:

```bash
# Regenerate zz_generated.deepcopy.go
make generate

# Regenerate config/crd/ and config/rbac/role.yaml
make manifests
```

`controller-gen` is invoked via `go run` at version `v0.14.0` — no global install required. Both commands must be re-run before committing marker changes. CI should validate that the generated files are not stale.

---

## Manager Flags

| Flag | Default | Description |
|---|---|---|
| `--metrics-bind-address` | `:8080` | Address the Prometheus metrics endpoint binds to |
| `--health-probe-bind-address` | `:8081` | Address the liveness/readiness probe endpoints bind to |
| `--leader-elect` | `false` | Enable leader election (required when running multiple replicas) |
| `--zap-log-level` | `info` | Log verbosity (`debug`, `info`, `error`) |
| `--zap-devel` | `true` (in code) | Development mode logger (human-readable output). Set `false` for JSON logs in production |

Leader election uses the ID `rector.apps.rector.io` and requires a `Lease` resource in the manager's namespace. Grant the manager ServiceAccount `leases` `get;list;watch;create;update;patch;delete` on `coordination.k8s.io` when `--leader-elect=true`.
