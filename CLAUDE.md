# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make generate          # regenerate zz_generated.deepcopy.go via controller-gen
make manifests         # regenerate config/crd/ and config/rbac/ from marker comments
make build             # compile bin/manager (runs generate + fmt + vet first)
make run               # run controller locally against current kubeconfig
make test              # go test -race ./... with coverage report
make install           # kubectl apply CRDs to current cluster
make uninstall         # kubectl delete CRDs
make docker-build      # build controller image (IMG=rector-controller:latest)
```

Run a single test:
```bash
go test -race -run TestReconcile ./internal/controller/...
```

## Architecture

**CRD group/version:** `apps.rector.io/v1alpha1`, kind `Application`

The controller owns two child resources per `Application` CR: a `Deployment` and a `Service`. Both are created with `ctrl.SetControllerReference` so Kubernetes GC cascades on deletion — no finalizers needed.

```
api/v1alpha1/
  application_types.go      — ApplicationSpec / ApplicationStatus types + kubebuilder markers
  groupversion_info.go       — SchemeBuilder registration (group: apps.rector.io)
  zz_generated.deepcopy.go  — generated; do not edit (regenerate with `make generate`)

internal/controller/
  application_controller.go — reconcile loop: CreateOrUpdate Deployment + Service, patch status

cmd/
  main.go                   — manager bootstrap, scheme registration, healthz/readyz probes

config/
  crd/                      — CRD manifest (regenerate with `make manifests`)
  rbac/role.yaml            — ClusterRole (regenerate with `make manifests`)
  samples/                  — example Application CR
```

## Key implementation notes

- **Status writes** use `r.Status().Patch()` in a deferred call; `r.Update()` will not persist status because the status subresource is enabled.
- **Service ClusterIP** is preserved in the `CreateOrUpdate` mutate func — overwriting it causes an immutability error.
- **Watch chain:** `.Owns(&appsv1.Deployment{})` and `.Owns(&corev1.Service{})` in `SetupWithManager` means changes to child resources re-enqueue the parent `Application`.
- **Conditions** use `k8s.io/apimachinery/pkg/api/meta.SetStatusCondition` with standard `metav1.Condition` — types are `Available` and `Progressing`.
- `make manifests` must be re-run after any change to `+kubebuilder:` markers in `api/v1alpha1/`.
