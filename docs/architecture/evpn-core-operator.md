# EVPN Core Operator lifecycle

The Operator is an experimental Kubernetes lifecycle controller for `EvpnCore`,
not a generic CNI or application-workload installer. The shipped
[`core-resources.yaml`](../../deploy/operator/core-resources.yaml) defines an
OVN/OVS Service and Deployment plus an optional Node-Agent DaemonSet in
`managed-single-node`; `external-ovn` manages only the Node-Agent DaemonSet.
The controller creates required namespaces but does not manage application
workloads or the separate legacy Kind compatibility services. The Framework
and Node-Agent retain endpoint/network and Type-2 runtime responsibilities.
This page is ordinary Markdown suitable for a future MkDocs Material
navigation tree; no website deployment is required.

The [release scope](../release/v0.1.0.md) describes integration maturity and
limitations. Experimental implementation does not establish general
multi-worker lifecycle or production readiness.

For the wider SDN framework and the separation between owned networking logic,
platform orchestration and external providers, see the
[framework ownership matrix](framework-architecture.md) and
[modular diagram](../uml/evpn-modular-architecture.svg).

## Architecture and ownership

The namespaced API is `evpn.romanspb80.github.io/v1alpha1`, kind `EvpnCore`
(`evpncores`, short name `ecore`). Sources:

- [Go API and deep copies](../../deploy/operator/api/v1alpha1/evpncore_types.go)
- [Reconciler and conditions](../../deploy/operator/controllers/evpncore_controller.go)
- [Installation lock, apply journal and cleanup](../../deploy/operator/controllers/lifecycle.go)
- [YAML rendering](../../deploy/operator/controllers/manifest.go) and
  [workload readiness](../../deploy/operator/controllers/readiness.go)
- [CRD, RBAC and operator Deployment](../../deploy/operator/manifests/operator.yaml)

The controller decodes YAML documents, relocates app/OVN namespaces, applies
configured images and Node-Agent arguments, and creates or patches objects.
The core YAML is an operator template, not a standalone installer; do not
apply it directly alongside an EvpnCore-managed installation.
Services keep API-server allocations such as ClusterIP. A three-way JSON
merge patch uses the installation's last-applied annotation to remove
previously declared fields without replacing unrelated top-level fields.
No-op reconciliations skip writes, including unchanged status.
Declared container lists are operator-controlled; arbitrary sidecar/list
injection is not a supported customization interface.

Only explicitly changed Deployment `GOBGP_ENDPOINT` values are retained for
legacy provider integration. The operator records template environment
defaults separately, so an old unchanged default cannot mask a new template.
Other environment values are declarative, including OVN/OVSDB dependencies:
namespace changes **and rollbacks to defaults** always regenerate their DNS
endpoints. Node-Agent arguments, images and
scheduling are declarative spec fields, not preserved out-of-band overrides.
Other Deployment settings remain YAML-driven.

Deployments and DaemonSets trigger reconciliation; all installations are
queued so singleton contenders can observe creation/deletion. A 30-second
resync covers other managed-resource drift and lock/ownership recovery.
API errors return to controller-runtime for retry. Operator replicas use a
separate leader-election Lease in `evpn-operator-system`.

| Owner | Scope |
|---|---|
| EvpnCore | `managed-single-node`: OVN/OVS Service and Deployment plus optional Node-Agent DaemonSet; `external-ovn`: Node-Agent DaemonSet only. Required namespaces are ensured but retained. Both use an installation lock and Kubernetes cleanup journal. No app workload is in the shipped template. |
| Independent k8gobgp installer/manager | Provider CRDs, DaemonSet, Service, speaker processes and `BGPConfiguration` |
| Framework/Node-Agent | Existing endpoint/attachment and optional Type-2 import/export lifecycle |
| OVN/OVS | Databases, logical-to-physical forwarding and controller-owned state |

Cross-namespace Kubernetes owner references are invalid for a namespaced
CR. Instead, every child has the installation CR's immutable UID in
`evpn.romanspb80.github.io/installation-uid` annotations and labels.
`status.managedResources` journals API version, kind, namespace, name and
observed child UID, `state` (`CreationPending`/`Observed`) and unresolved
`conflict` messages. Intent is persisted before creation; UID is recorded
afterward. A nonempty UID is never rebound to an unexpected replacement.
Confirmed absence permits a separately persisted recreation intent.
This allows restart-safe cleanup and namespace changes.
Existing unowned resources at desired keys are conflicts: no automatic
adoption, overwriting or takeover is performed.

In `managed-single-node` mode the controller ensures the configured app and
OVN namespaces exist, but does not delete them during finalization. Its
template deploys one OVN/OVS Service and Deployment, and creates the
Node-Agent DaemonSet only when enabled. In `external-ovn` mode it ensures only
the app namespace and manages only the Node-Agent DaemonSet; OVN/OVS and its
namespace are external. Neither profile deploys application workloads. The
Operator does not own separately installed Multus, NADs, CNI binaries,
provider resources, or an upstream OVN installation. CNI operation requires
an enabled CNI-serving Node-Agent; standalone EVPN import/export modes do not
start that service.

Each reconciliation keeps immutable CR UID/generation/spec and rendered
desired resources. Fresh uncached reads and optimistic resourceVersion
patches protect journal, status and finalizer writes. Spec/deletion changes
abort the old reconciliation; they cannot cause its workloads to be reported
Ready for a new generation. Reads are repeated before pruning, each workload
mutation and status publication. Kubernetes does not offer a transaction
across CR and child writes: one already in-flight child write may finish
after intent changes, but its durable inventory is recovered by the next
reconciliation/finalizer.

Before pruning or finalization, label-filtered discovery across namespaces
recovers resources of five supported kinds: Services, ConfigMaps,
ServiceAccounts, Deployments and DaemonSets. At most 256 inventory entries
are supported; truncated/oversized discovery fails closed. Ownership must
match the CR UID and the child's last-applied journal; recovered entries
do not override a previously recorded nonempty UID. Conflicts remain in
inventory and block cleanup and namespace replacement workloads.

## Singleton installation

Exactly one installation can actively manage core resources in a cluster.
Additional CRs can be **created**, but cannot acquire the installation lock.
This is reconciliation-level enforcement, not admission-level singleton
validation; there is no singleton webhook.

Before first acquisition, the controller orders eligible CRs by creation
timestamp, then `namespace/name`. The oldest valid enabled CR is the candidate;
a finalizing installation remains eligible until cleanup completes. It
persists its finalizer, then atomically creates/updates the fixed Lease
`evpn-operator-system/evpn-core-installation`, using the CR UID as holder.
An established holder is never displaced by a newer observation.
Losers receive `Degraded=True`, `Ready=False`, reason `OwnershipConflict`,
and a Warning event; they manage no shared children.

The installation Lease deliberately has no expiration. Operator downtime
does not authorize another CR to take over live resources. Cleanup releases
the holder only after mandatory deletion completes. Never manually delete
the Lease or remove a finalizer to force takeover without first auditing
the recorded children. If the CR or operator namespace was forcibly removed,
an orphaned lock/resource inventory requires administrator recovery.
Run one logical operator deployment/leader-election domain per cluster.

## API reference

`spec.enabled` is required. Disabling it pauses reconciliation, retains
existing resources and the installation lock, and sets `Ready=False` with
reason `Disabled`. **Disable is not uninstall.** Delete the CR to uninstall.

| Field | Default / behavior |
|---|---|
| `appNamespace` | `evpn-system`; Node-Agent namespace. No app Service/Deployment is in the shipped template. |
| `ovnNamespace` | `ovn-system`; OVN/OVS Deployment and Service in `managed-single-node`; external in `external-ovn` |
| `appImage` | Retained compatibility field (`evpn4ovn/app:kind` default); no container in the shipped template consumes it, so changing it has no effect on current managed resources |
| `ovnImage` | `evpn4ovn/ovn-ovs:kind`; OVN/OVS container |
| `dataplaneMode` | `managed-single-node` by default; `external-ovn` explicitly selects externally provisioned OVN/OVS and an agent-only DaemonSet |
| `dataplaneNode` | Required when enabled in `managed-single-node`; exact existing, Ready, schedulable Kubernetes Node name. Must be omitted in `external-ovn` |
| `gobgpImage` | Retained deprecated compatibility field; **not consumed** because speaker image belongs to the independent provider |
| `bgp.provider` | `k8gobgp`; admission schema and runtime validation reject other values, including `standalone` |
| `nodeAgent.enabled` | `false` for compatibility; enabling creates one DaemonSet |
| `nodeAgent.image` | `evpn4ovn/node-agent:kind`; agent and dependency init container |
| `nodeAgent.nodeSelector` | `managed-single-node`: optional selector constrained to `dataplaneNode`. `external-ovn`: required nonempty Kubernetes label selector |
| `nodeAgent.tolerations` | Empty; applied to OVN/OVS and Node-Agent in `managed-single-node`, or the agent in `external-ovn` |
| `nodeAgent.mode` | `real-ovn-ovs`; `in-memory` is available only for diagnostics |
| `nodeAgent.networkBackend` | `linux-veth`; `in-memory` is diagnostic, not live forwarding |
| `nodeAgent.socket` | `/run/evpn-node-agent/agent.sock` |
| `nodeAgent.ovnNBEndpoint` | `unix:/var/run/ovn/ovnnb_db.sock` |
| `nodeAgent.ovsdbEndpoint` | `unix:/var/run/openvswitch/db.sock` |
| `nodeAgent.ovnSBEndpoint` | `unix:/var/run/ovn/ovnsb_db.sock` |
| `nodeAgent.exportConfigPath`, `remoteConfigPath` | Empty; optional existing node-local JSON files |
| `nodeAgent.exportStatusPath`, `remoteStatusPath` | Empty; optional node-local JSON status output files |
| `nodeAgent.exportInterval`, `remoteInterval` | `2s`; positive Go duration strings |
| `nodeAgent.exportTimeout`, `remoteTimeout` | `5s`; positive Go duration strings |
| `nodeAgent.verifyNetlinkExportProvenance` | `true`; supported secure profiles must keep provenance verification enabled. The benchmark-only bypass is not acceptable for deployment or release validation. |
| `nodeAgent.gobgpHostPath` | `/run/gobgp`; mounted read-only at `/var/run/gobgp` |

Socket, config and status file paths must be clean absolute paths beneath
`/run/evpn-node-agent/`. The host GoBGP directory must be an absolute clean
path other than `/`. Export/import configuration requires
`real-ovn-ovs`/`linux-veth`. `k8gobgp-system` is reserved and cannot be used
as an app/OVN namespace.

Namespace relocation updates the rendered OVN Service endpoints without
changing the Node-Agent's default node-local Unix NB/OVS/SB socket endpoints
or its rendered configuration. The lifecycle runner checks this distinction
along with managed-resource and ownership transitions; see
[`tools/run-operator-e2e.sh`](../../tools/run-operator-e2e.sh) and the
baseline's schema and unit-test coverage.

```yaml
apiVersion: evpn.romanspb80.github.io/v1alpha1
kind: EvpnCore
metadata:
  name: evpn-core
  namespace: evpn-system
spec:
  enabled: true
  dataplaneNode: worker-1
  appNamespace: evpn-system
  ovnNamespace: ovn-system
  ovnImage: evpn4ovn/ovn-ovs:kind
  bgp:
    provider: k8gobgp
  nodeAgent:
    enabled: true
    image: evpn4ovn/node-agent:kind
    # Optional: stage identity-correct JSON on the designated node.
    # exportConfigPath: /run/evpn-node-agent/export-config.json
    # remoteConfigPath: /run/evpn-node-agent/remote-config.json
```

## Node-Agent runtime and configuration

The DaemonSet runs the unchanged
[`evpn-node-agent` CLI](../../cmd/evpn-node-agent/main.go). It uses host
networking and host PID namespace, root/privileged execution, and the
following host mounts:

| Host path | Container path | Purpose |
|---|---|---|
| `/run/openvswitch` | `/var/run/openvswitch` | Local OVSDB socket |
| `/run/ovn` | `/var/run/ovn` | NB/SB database sockets |
| `/run/evpn-node-agent` | Same | CNI socket, runtime JSON config/status |
| `/run/netns` | Same, `HostToContainer` propagation | CNI network namespaces |
| Configured GoBGP directory | `/var/run/gobgp`, read-only | Node-local speaker gRPC socket |
| Optional `type2JournalHostPath` | `/var/lib/evpn-node-agent` | Existing node-local directory for durable journals |

Host PID is needed for the exporter's real Unix peer pidfd/process evidence,
not merely for convenience. The host must support the kernel capabilities
already required by the existing exporter. Socket access remains `0600`.
The supplied [Node-Agent Dockerfile](../../deploy/operator/Dockerfile.node-agent)
provides the executable, shell, iproute2 and Unix-capable `nc`.

The dependency init container waits up to 180 seconds per attempt for local
OVN/OVS sockets and configured readable JSON files, then exits nonzero so
Kubernetes can retry. TCP endpoints rely on the existing runtime's connection
errors/restarts. Agent readiness checks Unix socket connectivity, not Type-2
convergence or the full NodeAgentCNI API. Rolling update has zero surge to
avoid deliberately running two agent pods per node.

In the default `managed-single-node` mode, required
node affinity uses `metadata.name`, not a potentially shared hostname label,
to place the single-replica OVN/OVS Deployment and Node-Agent DaemonSet on
the same `spec.dataplaneNode`. Node readiness, cordoning, selector mismatch
and untolerated scheduling taints result in `PlacementPending`, not Ready.
The DaemonSet must report exactly one desired/updated/ready/available pod.
There is no operator-managed multi-chassis OVN installation, node failover or state-preserving
node migration. Treat changing the designated node as a disruptive lab
reinstall, never as an OVN state migration.

### Externally prepared shared OVN

The experimental `external-ovn` mode manages **only** the agent DaemonSet.
Administrators must provide shared OVN NB/SB connectivity, a local OVS and
ovn-controller on each selected node, the local `/run/openvswitch/db.sock`,
the CNI/Multus installation, and any required node-local configuration.
Only the local OVS Unix endpoint is accepted. Shared endpoint values and
selector are deployment configuration; config files at the same path on
different hosts must contain the correct node identity, chassis, local VTEP,
speaker attestation and ownership domain for that host. `NODE_NAME` comes
from the Downward API; it does not provision or authenticate those files.
Do not copy one node's identity or ownership journal to another. The optional
provider observer RBAC and attestation files must be provisioned independently
with appropriate node-specific authorization; the shipped single-node RBAC
example is not a multi-node authorization policy.

For Type-2, the same config paths select **different host-local files** on
each worker; they are not a cluster-wide inventory or uniqueness check. A
Kubernetes speaker attestation must match `NODE_NAME` and, when recovery is
configured, its node UID. CNI continuity also rejects a different `NODE_NAME`.
When an export configuration is supplied in `external-ovn` mode, the
Operator enables `--evpn-export-verify-local-chassis`. Before each export
reconciliation the agent reads the node-local OVSDB `Open_vSwitch` row and
requires its `external_ids:system-id` to equal the configured chassis name,
then requires exactly one matching, identified Southbound `Chassis` row.
The existing exporter checks the local VTEP is assigned on the host; it does
not require that address to equal the OVN Geneve encapsulation address.
Outside opted-in inventory trust recovery, if chassis evidence is lost after durable export recovery was initialized,
the exporter attempts to persist a permanent withdrawal-only fence and
withdraws acknowledged UUIDs through the existing attested speaker and
authoritative RIB gates. A failed operation is reported as unresolved and
receipts are retained; foreign routes are not withdrawn. The fence survives
restart and blocks new advertisements even if chassis evidence later returns.
There is currently no automatic fence reset or ownership migration; recovery
requires an explicit, independently verified operational procedure. If
recovery has not initialized, or a pending journal operation prevents the
fence being persisted, withdrawal remains unresolved; a restart after a
failed fence write is not guaranteed to preserve that loss observation.
Older binaries that do not recognize the withdrawal fence cannot safely
resume this journal; do not roll back to them while that fence is present.
Operators must not treat a blocked cycle or status as proof of route absence.
Export recovery also requires the CNI continuity and exporter installation,
node UID and node name to agree.

An administrator must still verify unique speaker and VTEP assignments across
workers. The chassis check does not authenticate the remote Southbound server
or prove the integrity of the local OVSDB; these are trusted deployment
dependencies. These checks do not yet constitute a complete multi-worker
Type-2 deployment.

An optional `spec.workerInventory` declares `clusterUID`, a positive `revision`
and `workers` in the format below. The Operator binds its installation UID,
requires that the declared names and UIDs exactly match the Nodes selected by
the DaemonSet selector, and validates and publishes canonical content and its
SHA-256 digest in `evpn-worker-inventory` in the app namespace. The normal
resource journal refuses adoption of a conflicting ConfigMap. It reserves
worker identity before publication. A stale revision or changed digest at the
same revision is rejected, and an existing worker's entire assignment is
immutable. Workers can be added without modifying existing assignments.

For a clean installation the Operator reserves identities, publishes the
inventory, pins the observed ConfigMap UID and renders the existing Node-Agent
gate in the DaemonSet. Existing ungated or differently pinned Node-Agent
DaemonSets, Deployments and active Pods block rollout as `MigrationRequired`;
they are not stopped or withdrawn. Publication alone does not fence them.
Inventory changes and node selection must not be used as automatic ownership
migration; drain and verify old ownership before removing a worker.

The opt-in Node-Agent gate uses in-cluster service-account credentials and
reads only `evpn-worker-inventory` in the pinned namespace. Explicit command
line options `--worker-inventory-namespace`, `--worker-inventory-uid`,
`--worker-inventory-cluster-uid`, `--worker-inventory-installation-uid`, and
`--worker-inventory-journal`, and `--worker-inventory-recovery-file` are supplied by the Operator; the UID is the
ConfigMap Kubernetes UID, not its inventory digest. The journal is
`/var/lib/evpn-node-agent/inventory` on the configured persistent host volume.
The gate
also requires both enabled Type-2 configs, CNI continuity with admission,
`--evpn-export-verify-local-chassis`, the local OVS socket and the SB endpoint.
It checks the publication identity, Operator apply journal, canonical digest,
node UID and local launch configuration, independently checks chassis and
speaker, then accepts the revision in the separate durable journal. Each
export and import cycle repeats the check, and each inventory-mode Add
revalidates authority and current endpoint ownership. Import is also gated
before Netlink ownership initialization. With the recovery-file option,
classified temporary failures require full independent revalidation and a
durable two-journal grant; integrity, identity and unknown failures persist a
security fence requiring separate host-administrator authorization.
Restoring inventory bytes or speaker health cannot release that fence.
The inventory denial can be recorded before export recovery initializes;
exact owned withdrawal still requires initialized, reconciled export
ownership and an attested speaker. Unresolved withdrawal is reported, not
assumed. See [Type-2 ownership and recovery](type2-restart-ownership.md) for the
fail-closed identity and receipt requirements. Without the recovery-file option, the conservative
legacy permanent-fence behavior remains.

The API server, Operator credentials, administrators and RBAC are the authority;
the UID pin and apply journal detect replacement and drift, not an authorized
but malicious writer. The gate does not stop or fence an older agent started
without these flags. Do not switch an existing deployment to inventory mode
until its ownership and withdrawal state has been explicitly resolved.
The Operator has no automated safe transition or authenticated per-agent
acceptance acknowledgement. `InventoryPublished` reports publication;
`InventoryRolloutReady` reports only DaemonSet rollout. `InventoryAccepted`
and `RuntimeReady` remain `Unknown`, and the installation remains
`Reconciling` even after rollout. Check each condition's `observedGeneration`;
an earlier generation is not current evidence. Pod readiness is not acceptance.

For legacy migration, a trusted administrator must independently stop the
old processes, resolve durable recovery and verify exact owned BGP withdrawals
and Netlink cleanup before removing old workload objects or resolving recorded
workload UIDs. Do not delete a finalizer, clear status, or broadly delete routes
as a shortcut. If ownership cannot be proven clean, keep migration blocked.

Before enabling this experimental publication, a trusted administrator must
create the persistent reservation ConfigMap once, replacing the UUID with the
stable cluster scope used by the inventory:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: evpn-worker-reservations
  namespace: evpn-operator-system
  labels:
    app.kubernetes.io/managed-by: evpn-core-operator
data:
  reservations.json: '{"version":1,"clusterUID":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","reservations":[],"heads":{}}'
```

Save the bootstrap manifest and use `kubectl create -f <bootstrap-file>` once,
not `apply` or `replace`: overwriting an existing reservation resource with
empty state destroys the retained history.

The Operator updates this resource with resourceVersion concurrency checks;
absence, a different cluster scope, or malformed content blocks publication.
Reservations remain when a worker disappears or the EvpnCore is deleted.
They reserve node name/UID, chassis, speaker router ID and VTEP, and bind all
other worker fields to the original installation and node. Shared journal
path names on different nodes are not globally unique identities. An
administrator may release a reservation only after independently stopping the
old agent and proving its routes, dataplane ownership and durable recovery
state are safely cleared. The Operator has no automated release or migration.
Deletion or replacement of the reservation ConfigMap, its namespace, or the
cluster destroys this history; restoring an empty resource is not a safe
release. The Operator does not independently pin this ConfigMap's UID or
detect replacement with a valid-looking empty bootstrap; it can accept that
replacement and cannot recover lost reservations. Administrators must prevent
replacement and back up and restore the original state with the cluster's
trusted state.

Trust rests on Kubernetes API authentication/TLS and administrator-controlled
RBAC: only trusted administrators may change EvpnCore, create or replace the
reservation ConfigMap, or grant access; only the trusted Operator and those
administrators may mutate publication and reservation resources. The current
Operator ClusterRole permits ConfigMap create without resource-name restriction
(Kubernetes RBAC cannot constrain create by name), and update/patch only for
the inventory and reservation names, and delete only for the inventory name,
across namespaces. Historical managed ConfigMaps with other names require
administrator-granted permissions for their cleanup. Labels and the digest are not
authentication. API readers, local OVS/OVN and independently attested speaker
evidence remain separate trust requirements. The administrator-installed
`provider-attestation.yaml` includes a read-only, resource-name-scoped Role for
the inventory reader; adjust its namespace and ServiceAccount binding
if the app namespace differs from `evpn-system`; node `get` access must also
cover both actual worker names. Add Pod `get` in every namespace whose
workloads use the CNI; it is distinct from the provider Pod `list` permission.

[`worker-inventory-publication.yaml`](../../deploy/operator/examples/worker-inventory-publication.yaml)
is an inventory-enabled EvpnCore template, not a complete deployment. Replace its illustrative
UIDs with `kubectl get node <name> -o jsonpath='{.metadata.uid}'`, and replace
the other example identities and endpoints before `kubectl apply -f` on the
edited file. After the Operator reconciles, inspect
`kubectl -n evpn-system get configmap evpn-worker-inventory -o yaml` and
`kubectl -n evpn-system describe evpncore evpn-core`. The Operator deploys
the gated DaemonSet for a clean install; this manual template workflow is not
itself an accepted end-to-end procedure.

For a clean installation, prepare the external shared NB/SB domain, local OVS
and OVN controllers, node-local GoBGP sockets and persistent journal directories
first. Provision distinct speaker/router IDs, VTEPs and peers; place each
worker's export, import, CNI-continuity and provider-attestation JSON in
root-owned, non-group-writable paths. Those files must match the worker entry,
actual Node UID and the EvpnCore UID allocated by Kubernetes. Use the existing
[continuity configuration](cni-creation-authority.md) and
[provider attestation](type2-restart-ownership.md) contracts; boot ID,
network-namespace identity, executable hashes and runtime inspection paths
must be observed on each node, not copied from another. This manual workflow
is an experimental template, not a complete installer; see its
[validation procedure](../../deploy/operator/examples/two-worker/VALIDATION.md).
The [two-worker templates](../../deploy/operator/examples/two-worker/README.md)
describe the native OVN Netlink synchronization and controller-owned OVS
VXLAN forwarding path, node-local trust and external peer setup. The shared
EVPN-enabled switch requires explicit configuration and independent packet
checks; generalized setup, lifecycle and recovery are not guaranteed.

After creating the CR and provisioning those files, inspect the rollout and
the identity pin (substitute the actual namespaces and names):

```sh
kubectl -n evpn-system get configmap evpn-worker-inventory -o jsonpath='{.metadata.uid}{"\n"}'
kubectl -n evpn-system get daemonset evpn-node-agent -o jsonpath='{.spec.template.spec.containers[0].args}{"\n"}'
kubectl -n evpn-system get evpncore evpn-core -o jsonpath='{.status.conditions}{"\n"}'
kubectl -n evpn-system rollout status daemonset/evpn-node-agent
kubectl auth can-i get configmap/evpn-worker-inventory -n evpn-system --as=system:serviceaccount:evpn-system:evpn-provider-observer
kubectl auth can-i get node/worker-a --as=system:serviceaccount:evpn-system:evpn-provider-observer
kubectl auth can-i get node/worker-b --as=system:serviceaccount:evpn-system:evpn-provider-observer
```

These checks do not prove durable acceptance or route convergence. RBAC checks
may require impersonation privileges. The administrator must also grant
`get pods` for CNI workload namespaces and provision the immutable speaker
attestation and external peer configuration.

The following illustrates the canonical two-worker model. In an EvpnCore,
place `clusterUID`, `revision` and `workers` under `spec.workerInventory`;
the Operator supplies `version` and `installationUID`. UUIDs, addresses and
paths are illustrative. This is **not a deployable Type-2 configuration**.

```json
{
  "version": 1,
  "revision": 1,
  "clusterUID": "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
  "installationUID": "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
  "workers": [
    {
      "nodeName": "worker-a", "nodeUID": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
      "chassis": "chassis-a", "speakerRouterID": "192.0.2.11", "speakerASN": 64512,
      "speakerEndpoint": "unix:///var/run/gobgp/api.sock", "localVTEP": "192.0.2.21",
      "attestationPath": "/etc/evpn/provider-attestation/provider.json",
      "authorizationPath": "/run/evpn-node-agent/cni.json",
      "exportJournal": "/var/lib/evpn-node-agent/export",
      "importJournal": "/var/lib/evpn-node-agent/import",
      "continuityJournal": "/var/lib/evpn-node-agent/cni",
      "networks": [{"id": "tenant-a", "logicalSwitch": "tenant-a", "vni": 100}]
    },
    {
      "nodeName": "worker-b", "nodeUID": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
      "chassis": "chassis-b", "speakerRouterID": "192.0.2.12", "speakerASN": 64512,
      "speakerEndpoint": "unix:///var/run/gobgp/api.sock", "localVTEP": "192.0.2.22",
      "attestationPath": "/etc/evpn/provider-attestation/provider.json",
      "authorizationPath": "/run/evpn-node-agent/cni.json",
      "exportJournal": "/var/lib/evpn-node-agent/export",
      "importJournal": "/var/lib/evpn-node-agent/import",
      "continuityJournal": "/var/lib/evpn-node-agent/cni",
      "networks": [{"id": "tenant-a", "logicalSwitch": "tenant-a", "vni": 100}]
    }
  ]
}
```

The following is an actual Operator configuration fragment, not a complete
two-worker manifest: it enables delivery of separately provisioned host
files and persistent storage, but does not create or validate their contents.
The host journal directory must already exist on each node. Configure distinct
subdirectories under `/var/lib/evpn-node-agent` in the export, import and CNI
continuity files; mount the host directory on durable, node-local storage.
The operator does not check that the JSON actually uses this mount.

```yaml
spec:
  enabled: true
  dataplaneMode: external-ovn
  nodeAgent:
    enabled: true
    nodeSelector:
      example.com/evpn-ready: "true"
    ovnNBEndpoint: tcp:ovn-nb.example.internal:6641
    ovnSBEndpoint: tcp:ovn-sb.example.internal:6642
    exportConfigPath: /run/evpn-node-agent/export.json
    remoteConfigPath: /run/evpn-node-agent/import.json
    cniContinuityConfigPath: /run/evpn-node-agent/cni.json
    providerAttestation: true
    providerCTRHostPath: /opt/evpn/bin/ctr
    gobgpHostPath: /run/gobgp
    type2JournalHostPath: /var/lib/evpn-worker
```

For example, worker A and worker B each need their own root-owned export,
import, continuity and provider-attestation files at those respective paths,
their own local GoBGP socket, distinct VTEPs and recovery node UIDs, and
authorized network/VNI mappings. An administrator provisions and peers each
speaker with the external fabric; the Operator does not configure BGP peers.
The exporter uses the existing local endpoint observations and the importer
uses the existing policy-filtered RIB and owned Netlink path. Native OVN
Geneve remains responsible for ordinary connectivity within the shared OVN
domain. The template does not synthesize a complete set of node-local JSON
configurations or establish a supported deployment procedure; follow its
[validation guide](../../deploy/operator/examples/two-worker/VALIDATION.md).

Labels must be controlled by trusted administrators and assigned only after
the node is prepared. Kubernetes schedules the DaemonSet as labels/nodes
change and rolls it with zero surge. Empty selection is not Ready; Ready
indicates only the Kubernetes rollout, not full node inventory, OVN, BGP,
endpoint or route convergence. NotReady or unreachable nodes may retain
running processes and network state. Pod replacement and operator restart do
not transfer route ownership; recovery uses the existing node-local
identity, journal, fencing and exact receipts.

Before removing a label, changing the selector, replacing a node or deleting
the CR, an administrator must drain endpoints and verify exact route and
dataplane cleanup on every affected node. If a node is unreachable or an
AddPath outcome is ambiguous, preserve its evidence and treat cleanup and
ownership as unresolved; do not transfer or guess ownership. The finalizer
and installation Lease cover Kubernetes resources only: their completion or
release is **not** evidence of successful node-local cleanup. They do not
enforce this manual drain. Changing `dataplaneMode` on an existing installation
is rejected; perform a planned drain and reinstall instead. Back up the CR
status and upgrade the CRD together with the operator; rolling back to an
operator or schema unaware of the recorded mode is unsupported.

General multi-node CNI lifecycle and restart/recovery remain outside the
release guarantees. Shared-OVN Geneve is not evidence of independent OVN
domains exchanging EVPN Type-2/VXLAN.

Exporter config is loaded/reloaded by
[`LoadExporterConfig`](../../pkg/nodeagent/export_runtime.go);
remote config uses
[`LoadRemoteRuntimeConfig`](../../pkg/nodeagent/remote_runtime.go).
These preserve the existing case-sensitive JSON schema, strict validation,
node/chassis/VTEP/ASN/router-ID checks, route policies and exclusive writer
requirements. They are **not synthesized from the CR**. Stage each node's
distinct files before enabling their paths, preferably with atomic file
replacement. Export reload has identity-change restrictions; remote import
is launch-bound, so config changes can require a deliberate agent restart.
There is no ConfigMap-to-node identity renderer or automatic file-change
rollout in this MVP.

The CR provider setting does not rewrite `GRPCEndpoint` inside JSON.
Enabled remote import requires an explicit node-local Unix endpoint.
Node-Agent export/import remains opt-in and asynchronous. The standalone
OVSDB importer (`evpn-ovsdb`) is a separate component and is not replaced
by Node-Agent remote Netlink import.

Recognizable independent Node-Agent Deployments/DaemonSets (known names,
labels or explicit executable command) block enabling operator deployment
cluster-wide. There is no automatic adoption. Arbitrarily renamed image
entrypoints or host processes cannot reliably be discovered by this check;
administrators must ensure no independently launched agent shares the host
socket or writer authority.

## BGP provider boundary and future standalone

`k8gobgp` is the only provider accepted by the current Operator. GoBGP is the
BGP/EVPN engine; `k8gobgp` is an optional, independent Kubernetes speaker
configuration/lifecycle provider. Install its upstream release/CRDs and
authoritative `BGPConfiguration` separately. The current Operator template
does not create application clients or a `k8gobgp` Service. The separate,
unsupported legacy Kind compatibility manifest contains clients that may
reference `k8gobgp.k8gobgp-system.svc.cluster.local:50051`; only that legacy
path requires a separately reachable listener/Service.
The known upstream Unix-only runtime is not magically converted into a
TCP proxy by a Service manifest. See the
[provider ownership and reproducibility boundary](runtime-dependencies.md).

No GoBGP session/configuration writer is added. Existing Framework Type-2
AddPath/UUID DeletePath behavior and gRPC integration are unchanged.
Legacy speaker, peer-configuration and Type-2 route-write RPCs are disabled
with `PermissionDenied`; they are not configuration authority. The remaining
compatibility API still lacks transport authentication and authorization;
keep it isolated as described in [SECURITY.md](../../SECURITY.md).

Future `standalone` should provide an explicit speaker deployment/configuration
adapter with its own authoritative configuration ownership and readiness.
It must keep speaker ASN/router ID/peers/VRF configuration separate from
EVPN network intent and endpoint route lifecycles, and must never run
alongside a competing k8gobgp configuration writer for the same speaker.
Deployment/config schema, persistence, credentials and migration contracts
must be defined before enabling this Operator provider adapter.
Setting `spec.bgp.provider` to `standalone` is currently rejected; this is
distinct from
the opt-in [standalone Node-Agent export path](standalone-endpoint-authorization.md).

## Status and deletion

`status.phase`, compatibility `ready`/`reason`, `observedGeneration` and
Kubernetes-style conditions are updated together:

| Phase / true condition | Meaning |
|---|---|
| Reconciling | Workload rollout/pruning pending, no scheduled agents, or explicitly disabled |
| Ready | Every managed Deployment/DaemonSet has observed current generation and completed its rollout |
| Degraded | Ownership conflict, API/apply failure, or failed Deployment rollout |
| ConfigurationInvalid | Runtime spec validation failed; no new resources are applied |
| Deleting | Mandatory Kubernetes cleanup is in progress |

Successful manifest application is never sufficient for Ready. Deployment
replica counts must match the desired nonzero count. DaemonSet desired,
updated, ready and available counts must match, without unavailable or
misscheduled pods. BGP adjacency/RIB/traffic and database health are not
included. A pending DaemonSet does not currently have a custom rollout
timeout and can remain Reconciling.
Deployment failure conditions are interpreted only after its controller
has observed the workload's generation; old deadline/replica failures do
not classify a newly corrected, unobserved rollout as Degraded. Readiness
also checks installation ownership and child UID before accepting counts.

Conditions are persisted observations, **not a heartbeat**. While the
operator is unavailable they can remain stale indefinitely, and finalizers
make no progress. Consumers must compare condition/status
`observedGeneration` to CR `metadata.generation` and separately monitor
operator availability, Node/workload health and EVPN convergence.

Finalizer `evpn.romanspb80.github.io/core-cleanup` drives foreground deletion
of journaled installation-owned Kubernetes objects across namespaces.
UID/resourceVersion delete preconditions protect against replacements and
concurrent changes. Missing objects are success; changed ownership/UID is
retained **in the inventory**, reported with a Warning event and treated as
incomplete cleanup; the finalizer and installation Lease are retained.
Any mandatory cleanup/API
failure keeps the finalizer and retries. Obsolete resources are pruned
with the same rules after namespace changes or `nodeAgent.enabled=false`.
Namespace relocation waits for old workloads to be deleted before launching
replacements, avoiding overlapping node agents/host ports at the expense
of deliberate downtime.
Keep the operator and CRD installed until CR deletion finishes.

Namespaces are shared infrastructure and are always retained. k8gobgp
resources are outside the journal/cleanup allowlist. No host files,
OVN logical networks, OVS bridges, route receipts, database files or
persistent volumes are explicitly destroyed by the finalizer.
Deleting a workload still stops its processes and can interrupt forwarding.
Remove endpoint workloads and confirm their normal route withdrawal/cleanup
before uninstalling the core; the finalizer is not an EVPN RIB flush.
Host socket mounts are **not** a database durability/backup policy; the
existing OVN image startup and container storage can reset state on restart.
No destructive network-state deletion policy is implemented.

## Installation, upgrade and limitations

Use only trusted, isolated disposable hosts until security and operational
validation is complete. Existing unauthenticated management gRPC, OVN/OVSDB TCP
listeners, cluster-internal compatibility Services, privileged pods, and broad
cross-namespace permissions remain experimental assumptions. The legacy Kind
management Services are ClusterIP-only, with no management host networking,
NodePorts, or Gateway routes; unauthenticated in-cluster access remains.
Firewall/isolate those interfaces **before** applying the manifests; consult
[SECURITY.md](../../SECURITY.md).

### Administrator-only trust boundary

EvpnCore create/update/patch permissions are **cluster-administrator-equivalent**:
an author can select privileged images, host mounts and workload placement
across namespaces. Namespace ownership does not limit this authority.
Do not grant these permissions to tenants or ordinary namespace editors.
The shipped `evpn-core-administrator` ClusterRole is deliberately **unbound**
and has no aggregation labels. Explicitly bind it only to a reviewed,
trusted administrator identity; existing cluster administrators already
have this authority. Default bindings reference only the operator service
account. Generic namespace `edit`/`admin` roles do not aggregate these CR
permissions from this installation.

The operator account remains highly trusted: Kubernetes RBAC cannot restrict
its CR patch permission to only finalizer fields, and cross-namespace
workload management is inherently powerful. Its role omits CR create/delete
and unused pod reads; Lease access is restricted to `evpn-operator-system`.
Protect that service account, operator namespace, CRD, roles/bindings and
image supply chain. Audit existing cluster RBAC for pre-existing wildcard
or explicit grants: these manifests cannot revoke external bindings.
There is no tenant-safe delegation/admission policy in this MVP.

Privileged/root execution and hostPID remain necessary for the current
exporter peer-process/pidfd checks and host network/netns runtime,
not a claim that every mode requires every Linux capability. A reduced
NET_ADMIN/SYS_ADMIN profile, hostPID-free diagnostic mode and narrower
mounts need separate kernel/runtime validation; they are not introduced
as untested replacements here.

Build the Operator and the OVN/OVS and Node-Agent images required by the
chosen profile; the shipped template has no application image to build, and
`appImage` is retained but has no effect on current resources. Install
`k8gobgp` independently, apply `deploy/operator/manifests/operator.yaml`,
create the namespace that will contain the CR, stage node-local JSON if used,
specify `dataplaneNode` for `managed-single-node`, then apply the CR. The
controller ensures its configured app namespace and, in `managed-single-node`,
the OVN namespace. Watch conditions/events and actual runtime health. Image
changes roll managed workloads; OVN updates remain disruptive (`Recreate`).
`managed-single-node` CRs without `dataplaneNode`, or with arbitrary Node-Agent selectors,
become ConfigurationInvalid until an administrator supplies compatible
placement. Upgrade the CRD together with the operator so `state`, `conflict`
and placement fields are not pruned by an older schema.

The previous placeholder API group `evpn.core.example.com` is intentionally
removed. There is no conversion/migration compatibility. Pause the old
operator, inventory/back up its unowned resources and network state, and
perform an explicitly planned reinstall. Merely applying the new CRD does
not transfer old ownership. Do not delete shared k8gobgp or persistent state
to resolve an adoption conflict. Old operator environment defaults were
ineffective and are removed; set image/namespace fields on the CR.

### Recovery runbook

**Normal uninstall/reinstall:** withdraw endpoints/routes through their
existing runtime first, take appropriate OVN/OVS/config backups, delete the
EvpnCore and wait for finalizer completion. Keep the operator, CRD and
operator namespace installed until deletion finishes. Verify that owned
workloads/pods are gone and the installation Lease holder is empty before
creating a new CR (a new CR has a different UID even at the same name).
Keep independently managed k8gobgp, namespaces and retained host files.
Never treat their retention as an automatically restored working dataplane.

**Interrupted reconciliation/operator restart:** restore the same operator
deployment and CRD; do not recreate the CR or clear the Lease. Pending
creation intents survive API failures. Created children include ownership
and last-applied annotations; discovery confirms their UID if the post-create
status write was interrupted. Missing objects are retried from confirmed
absence. Partial cleanup retains inventory and the Lease, and resumes after
access/API failures are corrected. Optimistic conflicts retry from fresh
intent rather than overwriting newer status or spec.

**Stale installation Lease after forced CR removal:** stop the operator
before manual recovery, prevent new installation changes, and inventory the
holder and all old CR UIDs. These commands are read-only; replace names
according to the actual installation:

```sh
kubectl -n evpn-operator-system get lease evpn-core-installation -o yaml
kubectl get evpncores.evpn.romanspb80.github.io -A -o yaml
OLD_UID='<exact-holder-UID>'
kubectl get deployments,daemonsets,services,configmaps,serviceaccounts -A \
  -l "evpn.romanspb80.github.io/installation-uid=${OLD_UID}" -o yaml
kubectl get pods -A -o wide
```

Archive this inventory plus preserved status/last-applied evidence. Inspect
each recorded object's current UID, owner annotation, namespace and running
pods. Audit host processes/sockets/ports on the designated node as well:
force-deleted or partitioned-node pods can have processes still running.
Resolve each predecessor explicitly (safe exact-object deletion or documented
external ownership disposition). Do not run a blanket label-based delete,
rewrite UIDs to adopt replacements, or delete k8gobgp/network state.

Only after mandatory predecessor cleanup and host-runtime audit succeed
may an administrator clear the **installation** Lease holder. Fetch its
resourceVersion and current holder immediately before a resourceVersion-
guarded patch or `kubectl edit`; abort if either changed. Do not remove or
modify the separate controller leader-election Lease. Restart the operator
and then reconcile the selected installation. There is **no TTL, automatic
expiration or automatic takeover**.

**Orphans and ownership conflicts:** `status.managedResources[].conflict`
and Warning events identify unresolved replacements/invalid evidence.
Restore the original object/evidence only if its recorded UID actually
matches, or remove the conflicting exact object after administrator review
and ensure its host processes are stopped. Absence resolves that entry on
retry. Do not clear a conflict merely by changing its journal UID. Discovery
can restore missing/partial journals from valid labeled children, but it
cannot find objects with both labels and inventory lost, arbitrary renamed
entrypoints, or host-only processes. If the original UID evidence is also
lost, administrator validation is required; ownership metadata is evidence,
not a cryptographic identity against trusted administrators.

**Manual finalizer removal:** this bypasses cleanup and can orphan privileged
workloads, leave shared sockets/ports occupied and retain a stale Lease.
Removing the finalizer is not uninstall or a safe unlock. If the operator
is unavailable, restore it to finish cleanup; only use manual removal as a
documented administrator disaster-recovery action after the above audit.
Deleting the CRD or operator namespace can similarly destroy recovery
evidence. Save it before taking either action.

**Rollback/OVN upgrades:** namespace rollback regenerates endpoints but
foreground deletion/recreation remains disruptive. Operator image rollback
is supported only to a version understanding this API/journal and placement
contract; retain the newer CRD/status evidence and never switch API groups
or replace the CR UID as a shortcut. OVN image rollout, node movement and
namespace relocation are **not state-preserving upgrades**: runtime socket
host mounts do not persist the databases in `/var/lib`. Back up/export state
and plan an explicit recovery/reinstall. Durable OVN storage, tested database
migrations and automatic state-preserving rollback remain unimplemented.

Unit/schema coverage and the Containerlab-backed
[`tools/run-operator-e2e.sh`](../../tools/run-operator-e2e.sh) and baseline
unit/schema coverage exercise managed resources and namespace transitions. Use those inventory
checks rather than inferring app workloads from legacy manifests.
Specialized Framework/CNI suites
retain their evidence-specific runtime deployments; never combine them with
an enabled operator-managed agent in the same lab. No production or
multi-chassis acceptance is implied. Remaining work includes durable OVN
storage/recovery, authentication/authorization/network isolation, narrower
namespace-scoped privilege boundaries, reproducible pinned images,
multi-node upgrade/failure/convergence testing, runtime
configuration distribution, observability and standalone-provider design.
