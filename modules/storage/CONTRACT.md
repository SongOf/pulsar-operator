# DiskFleet integration contract

This contract belongs to `modules/storage/` in the unified `pulsar-operator` repository. File paths below are relative to this module. Repository-level wrappers and deployment composition are documented in the root README.

Python 3.10+. Package `diskfleet`. Production storage uses OpenEBS LVM LocalPV CSI (`local.csi.openebs.io`), thick provisioning by default, `Retain`, `WaitForFirstConsumer`. Our per-node operator discovers disks and creates/reconciles Volume Groups; CSI owns LVs/filesystems/mounts. No custom CSI protocol implementation.

API field names camelCase. Lists: `{items: [], total: int, limit: int, offset: int}`. Errors JSON `{detail: string}`. API auth: optional token in demo, required bearer token in cluster. UI sessionStorage token entered by user; no baked-in secrets.

The endpoint paths below are served directly by the storage API. The unified gateway exposes them under `/storage/api/` and removes `/storage` before forwarding; `/api/` on the gateway belongs to the Pulsar API. The two applications retain separate sessionStorage token keys.

- GET /api/config: `{mode: "demo"|"cluster", tokenRequired: bool}`
- GET /api/overview: `{mode, totals: {nodes,disks,pools,volumes,capacityBytes,allocatedBytes,availableBytes}, recentEvents:[], alerts:[]}`
- GET /api/nodes: paginated `{name,zone,state,disks,capacityBytes,allocatedBytes}`
- GET /api/disks?q=&node=&state=&limit=50&offset=0: `{id,name,node,path,serial,sizeBytes,media,model,state,reasons:[],poolId}`; state Available/Managed/Protected/Offline
- GET /api/pools: `{id,name,node,vgName,capacityBytes,allocatedBytes,availableBytes,state,diskIds:[],storageClass,createdAt}`
- POST /api/pools/plan `{name,node,diskIds:[]}` -> `{planId,expiresAt,commands:[],warnings:[],confirmation,disks:[]}`
- POST /api/pools `{planId,confirmation}` -> pool; demo immediate Ready, cluster Pending
- GET /api/volumes: `{id,name,namespace,poolId,pool,node,sizeBytes,status,storageClass,pvName,mountPath,createdAt}`
- Cluster volume responses additionally expose `pvcUid`, `management: diskfleet|external`, `canManage: bool`, `owners: [{kind,name,uid}]`, `bindingVerified: bool`, and `bindingMessage: string`. Bound volume location comes from the actual PV claim UID, CSI VG and node affinity; an unverified binding is read-only with an explanatory message. Pending volumes have no verified binding yet. External operator claims using verified DiskFleet classes are observable but never directly expanded/released. A Pulsar managed-by label is authoritative even if Retain leaves no ownerReferences. Shared classes use `storage.diskfleet.io/fleet=true` and a `storage.diskfleet.io/pool-uids` JSON annotation; see `diskfleet/fleet.py`.
- POST /api/volumes `{name,namespace,poolId,sizeGiB}` -> volume; demo Bound (simulated), cluster Pending PVC
- POST /api/volumes/{id}/expand `{sizeGiB}` -> volume
- DELETE /api/volumes/{id}?uid={pvcUid}: cluster clients send the UID they confirmed. Retain only; no data destruction or free capacity on deletion. A durable release intent is committed before DELETE and reconciled by original PVC/PV UID after a lost response. A name-only legacy retry is rejected after a completed release and name reuse; an old UID can return its own retained record but never delete the replacement. Demo requests omit uid.
- GET /api/events: `{id,time,level,action,message,node}`

Production CRDs cluster-scoped group `storage.diskfleet.io/v1alpha1`: PhysicalDisk and DiskPool. DiskPool spec: `nodeName`, `devices: [{path,serial,sizeBytes}]`, `vgName`, `approved: bool`. status: phase, capacityBytes, availableBytes, message, observedGeneration. Node label on objects `storage.diskfleet.io/node`. PhysicalDisk spec: nodeName,path,serial,sizeBytes,model,media; status: phase,reasons,poolName. Exactly one disk per DiskPool/VG in initial release for failure isolation. Batch fleet enrollment is one pool per disk, not cross-node VG.

Storage backend interface in diskfleet/storage.py: `discover_disks() -> list[dict]` camelCase disk fields (node assigned by caller); `LVMBackend(state_dir).ensure_pool(pool_name, devices, approved=False, owner_uid=..., allow_legacy_migration=False) -> dict` result capacityBytes,availableBytes,vgName. A nonempty DiskPool UID is required and stored in the host journal as poolUID. Safe list-argv execution, per-host file lock, stable by-id identity, explicit approval, system/used-disk rejection, no wipe/delete, durable ownership. The Agent permits legacy Ready-journal migration only with independently verified pool/StorageClass ownership; the backend still verifies PV UUID, VG tag and single-disk membership before adding the UID, without initialization.

Duplicate live DiskPool claims on the same node (VG, path or serial) are rejected regardless of node labels. The API projects these as Conflict with zero usable capacity and excludes their classes from trusted volume management, so legacy duplicate Ready records cannot double-count physical capacity.

Cluster integration in diskfleet/cluster.py reads/writes the Kubernetes API using explicit connection settings or a service account; it does not automatically load kubeconfig. The node operator is `python3 -m diskfleet.operator` and runs one instance per selected node, using NODE_NAME and ENABLE_DISK_WRITES=false by default. `diskfleet/api.py` owns HTTP delivery, `diskfleet/store.py` owns demo state and local plans/audit, and `diskfleet/static/` owns the storage UI. The repository root owns Pulsar controllers, the React UI, image orchestration and the shared gateway configuration.
