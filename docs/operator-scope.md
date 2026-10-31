# Pulsar / BookKeeper Operator Scope Assessment

This document evaluates the feature set required for an operator to fully take over Pulsar and BookKeeper cluster operations. It is intentionally broader than the current prototype so future implementation can be planned without hiding operational requirements behind a minimal CRD.

## Core principles

- Treat the operator as the source of truth for desired state, status, upgrades, and safe operations.
- Keep Pulsar and BookKeeper lifecycle models separate, but allow a Pulsar cluster to reference a managed BookKeeper cluster.
- Model every long-running operation as declarative intent with observable status, not only as an imperative button.
- Make all storage for BookKeeper journal and ledger data PV/PVC based, including expansion and replacement workflows.

## API surface

### PulsarCluster

Required spec areas:

- Global metadata: cluster name, namespace, labels, annotations, tenant/environment identity.
- Images and versions for broker, proxy, functions worker, autorecovery, tool/admin jobs, and optional ZooKeeper / metadata store components.
- Component replicas: broker, proxy, functions worker, optional recovery and coordination components.
- Component configuration maps:
  - broker.conf values, advertised listeners, protocol handlers, authentication/authorization flags.
  - proxy.conf values and external access configuration.
  - functions worker configuration, package management, runtime isolation, and worker service toggles.
- Resource policy per component: CPU/memory requests and limits, JVM heap/direct memory, pod priority, node selectors, tolerations, affinity, topology spread, disruption budgets.
- Networking: Services, ingress/gateway, TLS endpoints, internal and external listeners, advertised addresses, load balancer annotations, network policies.
- Security: TLS certificates, mTLS, token/JWT/OAuth2 configuration, super users, role bindings, secret references, service accounts and RBAC.
- Metadata dependencies: reference to an internal or external metadata store and BookKeeper cluster; migration and initialization jobs.
- Maintenance policy: safe restart order, rolling upgrade strategy, drain timeout, unavailable replica budget, maintenance windows.
- Operational intents: scale, rolling restart, config reload, namespace bundle unload/split, broker decommission, health repair, backup/restore hooks.

### BookKeeperCluster

Required spec areas:

- Replicas, image/version, service account/RBAC and scheduling constraints.
- Bookie configuration: ports, journal/ledger paths, GC compaction, entry log size, netty settings, read/write throttles, disk usage thresholds, rack/region placement metadata.
- Journal storage PVC template: storage class, size, access mode, volume mode, selector, retention policy.
- Ledger storage PVC template: storage class, size, access mode, volume mode, selector, retention policy.
- Multi-ledger disk support: multiple ledger PVC templates and mount paths.
- Volume expansion policy: online expansion support, file-system resize status, and manual approval gate when the storage class does not support expansion.
- Disk replacement policy: cordon bookie, drain ledgers, replace PVC, rejoin and verify under-replication recovery.
- AutoRecovery configuration and placement, including under-replicated ledger rereplication controls.
- Decommission and scale-in policy: enforce ensemble/write quorum safety, wait for ledger replication, and remove metadata only after verification.

## Reconciliation responsibilities

- Create/update StatefulSets, Deployments, Services, ConfigMaps, Secrets references, PodDisruptionBudgets, NetworkPolicies, ServiceMonitors and Jobs.
- Enforce immutable field handling by creating replacement resources when Kubernetes cannot patch a field safely.
- Maintain status conditions for Available, Progressing, Degraded, Scaling, Upgrading, StorageExpanding and OperationBlocked.
- Detect drift between desired and live Kubernetes objects, then repair or surface a blocked condition.
- Run bootstrap jobs for metadata initialization and admin tasks with idempotent completion markers.
- Coordinate ordered rolling restarts when config, image, certificates or JVM settings change.
- Gate destructive actions with safety checks and explicit operation status.

## Operational workflows

Minimum workflows for full ownership:

1. Create a Pulsar cluster with managed or external BookKeeper.
2. Create a standalone BookKeeper cluster.
3. Scale brokers, proxies, functions workers and bookies independently.
4. Scale in bookies only after ledger under-replication is cleared.
5. Expand journal and ledger PVCs and report whether the filesystem resize completed.
6. Replace failed bookie disks without losing ledger data.
7. Rolling restart by component and by ordinal.
8. Rolling upgrade and rollback for Pulsar and BookKeeper images/config.
9. Broker decommission / drain, namespace bundle unload and traffic movement.
10. Bookie decommission, recovery and rereplication monitoring.
11. Certificate and secret rotation with safe pod rollout.
12. Backup and restore integration for metadata and configuration.
13. Status dashboard and event/audit history for every operation.

## Frontend requirements

The web console should expose both desired state editing and operation execution:

- Overview dashboard: cluster health, component readiness, versions, capacity and recent operations.
- Create wizards for Pulsar and BookKeeper with validation for storage, security and networking.
- Detail pages per cluster with tabs for components, configuration, storage, networking, security, events and YAML.
- Scaling controls with safety warnings and status tracking.
- Storage management for BookKeeper journal and ledger PVCs, including expansion and disk replacement workflows.
- Configuration editor with diff preview, restart impact analysis and rollback to last applied settings.
- Upgrade workflow with image selection, preflight checks, progress, rollback and pause/resume.
- Operations audit log showing actor, requested change, affected resources, start/end time and result.

## Gaps in the current prototype

The current PR establishes a skeleton: CRD schemas, sample resources, a small management server and a React console mock workflow. It does not yet implement a production controller that reconciles Kubernetes workloads, status, safety gates, real Pulsar admin operations, or authenticated multi-user frontend workflows.

Production readiness requires adding controller-runtime reconciliation, generated clients, status subresource updates, workload manifests, admission validation, integration/e2e tests against Kubernetes, and real UI calls to the operator API.
