# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build             # compile bin/manager (runs fmt + vet first)
make run               # run controller locally against current kubeconfig
make test              # go test -race ./... with coverage report
make docker-build      # build controller image (IMG=cloudflare-controller:latest)
```

Run a single test:
```bash
go test -race -run TestReconcile ./internal/controller/...
```

## Architecture

No custom CRDs. The controller watches standard `core/v1` **Services** for `cloudflare-controller.io/*` annotations and manages external Cloudflare resources in response.

```
internal/
  cloudflare/
    client.go           — Client interface + cloudflare-go implementation
                          (EnsureDNSRecord, DeleteDNSRecord, EnsureAccessApp, DeleteAccessApp)
  configmap/
    manager.go          — cloudflared ConfigMap ingress upsert/remove; retries on 409 Conflict
  config/
    config.go           — YAML config loader (Load) + validator (Validate)
  controller/
    service_controller.go — ServiceReconciler: DNS + ConfigMap + Access App lifecycle

cmd/
  main.go               — manager bootstrap: load config, read CLOUDFLARE_API_TOKEN env var,
                          build cfc.Client, wire ServiceReconciler, start manager

config/
  rbac/role.yaml        — ClusterRole for the controller
  samples/
    controller-config.yaml  — example config file
```

## Key implementation notes

- **No CRDs.** All config is injected into `ServiceReconciler` as struct fields at startup (from YAML file + flag overrides + `CLOUDFLARE_API_TOKEN` env var).
- **Finalizer** `cloudflare-controller.io/finalizer` is added before any external mutations; cleanup runs on deletion or hostname annotation removal.
- **Annotation patch** uses `client.MergeFrom` so only changed annotations are written back — avoids clobbering other controllers.
- **ConfigMap retry**: `configmap.Manager` retries up to 3× on `409 Conflict` (optimistic locking).
- **CLOUDFLARE_API_TOKEN** is never in the config file — always from env var, mounted from a Kubernetes Secret.
- `GOMODCACHE=/tmp/gomodcache go mod tidy` if the default module cache is root-owned.
