# Context

Glossary for `kube-state-graph`. Terms only — no implementation details, no
design decisions. Those live in `openspec/` and `CLAUDE.md`.

## Storage domain

The word "cluster", "node" and "volume" each name two or three *different*
things across the Kubernetes and NetApp ONTAP sides. They are never
interchangeable.

### Kubernetes side

- **K8s cluster** — a Kubernetes cluster. The scope prefix on every graph node
  id, the values of the `clusters` response field, and the domain of the
  `?cluster=` filter. When this repo says "cluster" unqualified, this is it.
- **K8s node** — a Kubernetes worker/control-plane machine. Graph node
  `type="node"`.
- **PVC** — PersistentVolumeClaim. A namespaced claim. Graph node `type="pvc"`.
- **PV** — PersistentVolume. The cluster-scoped object a PVC binds to.
  Dynamically provisioned PV names are always of the form `pvc-<uuid>`.
  The PV is *not* a graph node; its name is carried on the PVC.
- **Pod volume** — the name a pod's own spec gives a mount. Distinct from both
  the PV and the ONTAP volume; a single PVC can be mounted under different pod
  volume names by different pods.

### NetApp ONTAP side

- **ONTAP cluster** — a NetApp ONTAP cluster. Unrelated to a K8s cluster; the
  two namespaces can and do collide on name.
- **NetApp node** — a controller/head in an ONTAP cluster. Unrelated to a
  K8s node.
- **Aggregate** — a pool of physical disks owned by exactly one NetApp node.
- **SVM** (Storage Virtual Machine, "vserver") — the tenancy unit that owns
  volumes and serves them over NFS/iSCSI.
- **FlexVol** — an ONTAP volume: the thing that actually backs a PV. Its name
  is **not** the PV name — ONTAP volume names admit only letters, digits and
  underscores, so a `pvc-<uuid>` PV name cannot be one. Trident derives the
  FlexVol name from the PV name and records it as the volume's
  **internal name**.
- **Internal name** — the FlexVol (or qtree/LUN, on the "economy" drivers)
  name Trident assigned for a given PV.
- **PVC-identity tag** — a value carried on the ONTAP volume that names the
  Kubernetes object it backs. Not intrinsic to ONTAP: it exists only where the
  provisioning backend was configured to write it.

### Cardinality

- One PVC binds one PV.
- On the standard drivers one PV maps to one FlexVol.
- On the **economy** drivers many PVs share one FlexVol, each as a qtree or
  LUN inside it — so per-PVC performance figures do not exist at the FlexVol
  level there.
- One FlexVol lives on one aggregate; one aggregate belongs to one NetApp node.
