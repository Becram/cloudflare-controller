# Graph Report - .  (2026-04-15)

## Corpus Check
- Corpus is ~11,204 words - fits in a single context window. You may not need a graph.

## Summary
- 111 nodes · 127 edges · 18 communities detected
- Extraction: 91% EXTRACTED · 9% INFERRED · 0% AMBIGUOUS · INFERRED: 12 edges (avg confidence: 0.85)
- Token cost: 0 input · 0 output

## Community Hubs (Navigation)
- [[_COMMUNITY_Cloudflare Resource Lifecycle|Cloudflare Resource Lifecycle]]
- [[_COMMUNITY_Cloudflare API Client|Cloudflare API Client]]
- [[_COMMUNITY_ConfigMap Manager|ConfigMap Manager]]
- [[_COMMUNITY_cloudflared Infra Manager|cloudflared Infra Manager]]
- [[_COMMUNITY_Configuration System|Configuration System]]
- [[_COMMUNITY_Service Reconciler|Service Reconciler]]
- [[_COMMUNITY_Secrets and Credentials|Secrets and Credentials]]
- [[_COMMUNITY_Service Annotations|Service Annotations]]
- [[_COMMUNITY_Project Overview|Project Overview]]
- [[_COMMUNITY_Controller Runtime|Controller Runtime]]
- [[_COMMUNITY_Config and Health|Config and Health]]
- [[_COMMUNITY_Main Entry Point|Main Entry Point]]
- [[_COMMUNITY_Docker Build|Docker Build]]
- [[_COMMUNITY_Annotation Patch Strategy|Annotation Patch Strategy]]
- [[_COMMUNITY_Project CLAUDE|Project CLAUDE.md]]
- [[_COMMUNITY_RBAC ClusterRole|RBAC ClusterRole]]
- [[_COMMUNITY_Argo Tunnel|Argo Tunnel]]
- [[_COMMUNITY_Requeue Interval|Requeue Interval]]

## God Nodes (most connected - your core abstractions)
1. `ConfigMap Management` - 9 edges
2. `ServiceReconciler Struct` - 8 edges
3. `ServiceReconciler` - 7 edges
4. `Manager` - 7 edges
5. `cfClient` - 6 edges
6. `Service Creation Data Flow` - 6 edges
7. `ServiceReconciler` - 5 edges
8. `Cloudflare Access Application` - 5 edges
9. `PolicySpec` - 4 edges
10. `Manager` - 4 edges

## Surprising Connections (you probably didn't know these)
- `ConfigMap Manager (manager.go)` --semantically_similar_to--> `ConfigMap Management`  [INFERRED] [semantically similar]
  CLAUDE.md → docs/ARCHITECTURE.md
- `ServiceReconciler` --semantically_similar_to--> `ServiceReconciler Struct`  [INFERRED] [semantically similar]
  CLAUDE.md → docs/ARCHITECTURE.md
- `Cloudflare Finalizer` --semantically_similar_to--> `Finalizer Pattern`  [INFERRED] [semantically similar]
  CLAUDE.md → docs/ARCHITECTURE.md
- `CLOUDFLARE_API_TOKEN Env Var` --semantically_similar_to--> `Secret Handling (CLOUDFLARE_API_TOKEN)`  [INFERRED] [semantically similar]
  CLAUDE.md → docs/ARCHITECTURE.md
- `Apache License 2.0` --references--> `System Overview`  [INFERRED]
  hack/boilerplate.go.txt → docs/ARCHITECTURE.md

## Hyperedges (group relationships)
- **Reconcile Lifecycle: DNS + ConfigMap + Access App** — arch_dns_record, arch_configmap_management, arch_access_app [EXTRACTED 0.95]
- **cloudflared Infrastructure Resources: Secret + ConfigMap + Deployment** — arch_cloudflared_infra_manager, arch_configmap_management, arch_rolling_restart [EXTRACTED 0.90]
- **Service Annotation Triggers Controller Watch and Reconcile** — readme_hostname_annotation, arch_watch_predicate, arch_service_reconciler_struct [EXTRACTED 0.92]

## Communities

### Community 0 - "Cloudflare Resource Lifecycle"
Cohesion: 0.14
Nodes (20): Cloudflare Access Application, cloudflare-go Library, cloudflared Infrastructure Manager, ConfigMap Management, Design Decision: ConfigMap as Ingress Source of Truth, Service Creation Data Flow, Service Deletion Data Flow, CNAME DNS Record Lifecycle (+12 more)

### Community 1 - "Cloudflare API Client"
Cohesion: 0.18
Nodes (4): isNotFound(), cfClient, Client, PolicySpec

### Community 2 - "ConfigMap Manager"
Cohesion: 0.26
Nodes (7): cloudflaredConfig, ingressRule, Manager, originRequest, insertBeforeCatchAll(), parseConfig(), removeByHostname()

### Community 3 - "cloudflared Infra Manager"
Cohesion: 0.33
Nodes (1): Manager

### Community 4 - "Configuration System"
Cohesion: 0.29
Nodes (6): CloudflareConfig, CloudflaredConfig, Config, ConfigMapRef, defaults(), Load()

### Community 5 - "Service Reconciler"
Cohesion: 0.39
Nodes (1): ServiceReconciler

### Community 6 - "Secrets and Credentials"
Cohesion: 0.29
Nodes (8): external-secrets (AWS SSM), Secret Handling (CLOUDFLARE_API_TOKEN), CLOUDFLARE_API_TOKEN Env Var, Cloudflare Client (client.go), Config Loader (config.go), ConfigMap Manager (manager.go), Manager Bootstrap (main.go), ServiceReconciler

### Community 7 - "Service Annotations"
Cohesion: 0.29
Nodes (7): Access Policies Sync, cloudflare-controller.io/access-enabled Annotation, cloudflare-controller.io/access-policies Annotation, gRPC / HTTP2 Service Usage Example, cloudflare-controller.io/hostname Annotation, cloudflare-controller.io/http2-origin Annotation, Service Annotations

### Community 8 - "Project Overview"
Cohesion: 0.4
Nodes (5): Design Decision: No Custom CRDs, Design Decision: Single Tunnel Per Controller, System Overview, Apache License 2.0, cloudflare-controller (README)

### Community 9 - "Controller Runtime"
Cohesion: 0.5
Nodes (4): controller-runtime Framework, Error Handling and Retries, Kubernetes Events, Prometheus Metrics

### Community 10 - "Config and Health"
Cohesion: 0.5
Nodes (4): Config Struct, Configuration System, Config File Structure, Health Probes (/healthz, /readyz)

### Community 11 - "Main Entry Point"
Cohesion: 0.67
Nodes (0): 

### Community 12 - "Docker Build"
Cohesion: 1.0
Nodes (2): Distroless Runtime Image, Multi-stage Dockerfile

### Community 13 - "Annotation Patch Strategy"
Cohesion: 1.0
Nodes (2): Annotation Patch Strategy (MergeFrom), Design Decision: client.MergeFrom for Annotations

### Community 14 - "Project CLAUDE.md"
Cohesion: 1.0
Nodes (1): cloudflare-controller Project

### Community 15 - "RBAC ClusterRole"
Cohesion: 1.0
Nodes (1): ClusterRole (role.yaml)

### Community 16 - "Argo Tunnel"
Cohesion: 1.0
Nodes (1): Cloudflare Argo Tunnel

### Community 17 - "Requeue Interval"
Cohesion: 1.0
Nodes (1): 5-Minute Requeue Interval

## Knowledge Gaps
- **35 isolated node(s):** `CloudflareConfig`, `CloudflaredConfig`, `ConfigMapRef`, `Client`, `cloudflaredConfig` (+30 more)
  These have ≤1 connection - possible missing edges or undocumented components.
- **Thin community `Docker Build`** (2 nodes): `Distroless Runtime Image`, `Multi-stage Dockerfile`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Annotation Patch Strategy`** (2 nodes): `Annotation Patch Strategy (MergeFrom)`, `Design Decision: client.MergeFrom for Annotations`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Project CLAUDE.md`** (1 nodes): `cloudflare-controller Project`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `RBAC ClusterRole`** (1 nodes): `ClusterRole (role.yaml)`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Argo Tunnel`** (1 nodes): `Cloudflare Argo Tunnel`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Requeue Interval`** (1 nodes): `5-Minute Requeue Interval`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `ServiceReconciler Struct` connect `Cloudflare Resource Lifecycle` to `Controller Runtime`, `Secrets and Credentials`?**
  _High betweenness centrality (0.058) - this node is a cross-community bridge._
- **Why does `Cloudflare Access Application` connect `Cloudflare Resource Lifecycle` to `Service Annotations`?**
  _High betweenness centrality (0.041) - this node is a cross-community bridge._
- **Are the 2 inferred relationships involving `ServiceReconciler Struct` (e.g. with `RBAC Model` and `ServiceReconciler`) actually correct?**
  _`ServiceReconciler Struct` has 2 INFERRED edges - model-reasoned connections that need verification._
- **What connects `CloudflareConfig`, `CloudflaredConfig`, `ConfigMapRef` to the rest of the system?**
  _35 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `Cloudflare Resource Lifecycle` be split into smaller, more focused modules?**
  _Cohesion score 0.14 - nodes in this community are weakly interconnected._