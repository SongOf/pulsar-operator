# pulsar-operator

A Kubernetes operator and web console for Apache Pulsar and BookKeeper cluster operations.

## Capabilities

- Defines `PulsarCluster` and `BookKeeperCluster` custom resources.
- Models create, scale out, scale in, restart/decommission, and storage update operations.
- Requires BookKeeper journal and ledger storage to be expressed as PV/PVC-backed sizes.
- Provides a React management console for viewing cluster status and executing common operations.
- Includes a lightweight Go management server with REST endpoints suitable for wiring to Kubernetes reconciliation.

## Quick start

```bash
go test ./...
npm install
npm run build
```

Apply CRDs and sample resources:

```bash
kubectl apply -f config/crd/
kubectl apply -f config/samples/
```

Run the management server after building the UI:

```bash
npm run build
WEB_ROOT=dist go run ./cmd/operator
```
