# EVPN Core Operator lifecycle

The operator is an experimental Kubernetes lifecycle controller, not a
production-ready EVPN installation. It manages Kubernetes workloads using
[`core-resources.yaml`](../../deploy/operator/core-resources.yaml), while
the existing Framework and Node-Agent retain endpoint/network and Type-2
runtime responsibilities. This page is ordinary Markdown suitable for a
future MkDocs Material navigation tree; no website deployment is required.

The [operator-managed Type-2 acceptance suite](operator-type2-acceptance.md)
exercises the actual CNI, direct Unix GoBGP integration, independent FRR peer
and captured VXLAN traffic. Its bounded Phase D normal and forced-SIGKILL
runs verify Node-Agent/speaker recovery; ambiguous lost-UUID AddPath outcomes
remain intentionally BLOCKED. Kubernetes workload readiness
must not be interpreted as proof of route/packet convergence.

For the wider SDN framework and the separation between owned networking logic,
platform orchestration and external providers, see the
[framework ownership matrix](framework-first.md) and
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
| EvpnCore | App and OVN Services/Deployments, optional Node-Agent DaemonSet; installation lock and cleanup journal |
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
| `appNamespace` | `evpn-system`; app Services/Deployments and Node-Agent |
| `ovnNamespace` | `ovn-system`; OVN/OVS Deployment and Service |
| `appImage` | `evpn4ovn/app:kind`; all compatibility app containers |
| `ovnImage` | `evpn4ovn/ovn-ovs:kind`; OVN/OVS container |
| `dataplaneNode` | Required when enabled; exact existing, Ready, schedulable Kubernetes Node name; no multi-node placement |
| `gobgpImage` | Retained deprecated compatibility field; **not consumed** because speaker image belongs to the independent provider |
| `bgp.provider` | `k8gobgp`; admission schema and runtime validation reject other values, including `standalone` |
| `nodeAgent.enabled` | `false` for compatibility; enabling creates one DaemonSet |
| `nodeAgent.image` | `evpn4ovn/node-agent:kind`; agent and dependency init container |
| `nodeAgent.nodeSelector` | Optional legacy hostname selector, only `kubernetes.io/hostname` equal to `dataplaneNode`; arbitrary/multi-node selectors are rejected |
| `nodeAgent.tolerations` | Empty; applied to both OVN/OVS and Node-Agent; explicitly configure control-plane/dedicated-node taints if needed |
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
| `nodeAgent.gobgpHostPath` | `/run/gobgp`; mounted read-only at `/var/run/gobgp` |

Socket, config and status file paths must be clean absolute paths beneath
`/run/evpn-node-agent/`. The host GoBGP directory must be an absolute clean
path other than `/`. Export/import configuration requires
`real-ovn-ovs`/`linux-veth`. `k8gobgp-system` is reserved and cannot be used
as an app/OVN namespace.

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
  appImage: evpn4ovn/app:kind
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

The current operator-managed MVP is explicitly **single-node**. Required
node affinity uses `metadata.name`, not a potentially shared hostname label,
to place the single-replica OVN/OVS Deployment and Node-Agent DaemonSet on
the same `spec.dataplaneNode`. Node readiness, cordoning, selector mismatch
and untolerated scheduling taints result in `PlacementPending`, not Ready.
The DaemonSet must report exactly one desired/updated/ready/available pod.
There is no multi-chassis OVN installation, node failover or state-preserving
node migration. Treat changing the designated node as a disruptive lab
reinstall, never as an OVN state migration.

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

`k8gobgp` remains the default, independent lifecycle owner. Install its
upstream release/CRDs and authoritative `BGPConfiguration` separately. The
core no longer creates or deletes a `k8gobgp` Service. Compatibility app
clients still reference
`k8gobgp.k8gobgp-system.svc.cluster.local:50051`; the provider installation
must explicitly supply a reachable listener/Service if using that path.
The known upstream Unix-only runtime is not magically converted into a
TCP proxy by a Service manifest. See the
[provider ownership and reproducibility boundary](runtime-dependencies.md).

No GoBGP session/configuration writer is added. Existing Framework Type-2
AddPath/UUID DeletePath behavior and gRPC integration are unchanged.
Compatibility management RPCs capable of configuring speakers still need
operational isolation from k8gobgp's manager; this task does not resolve
the broader [FW-02](../roadmap/backlog.md) contract.

Future `standalone` should provide an explicit speaker deployment/configuration
adapter with its own authoritative configuration ownership and readiness.
It must keep speaker ASN/router ID/peers/VRF configuration separate from
EVPN network intent and endpoint route lifecycles, and must never run
alongside a competing k8gobgp configuration writer for the same speaker.
Deployment/config schema, persistence, credentials and migration contracts
must be defined before enabling it. Currently `standalone` is rejected.

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
validation is complete. Existing management gRPC, OVN/OVSDB TCP listeners,
NodePorts `30052`/`30053`, privileged pods and broad cross-namespace
permissions remain experimental assumptions. Firewall/isolate those
interfaces **before** applying the manifests; consult
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
tested exporter peer-process/pidfd evidence and host network/netns runtime,
not a claim that every mode requires every Linux capability. A reduced
NET_ADMIN/SYS_ADMIN profile, hostPID-free diagnostic mode and narrower
mounts need separate kernel/runtime validation; they are not introduced
as untested replacements here.

Build compatible operator/app/OVN/Node-Agent images, install k8gobgp
independently, apply `deploy/operator/manifests/operator.yaml`, create the
CR's namespace, stage node-local JSON if used, specify the exact dataplane node,
then apply the CR. Watch conditions/events and actual runtime health.
Image changes roll workloads; OVN updates remain disruptive (`Recreate`).
Existing CRs without `dataplaneNode`, or with arbitrary Node-Agent selectors,
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
[`tools/run-operator-e2e.sh`](../../tools/run-operator-e2e.sh) test lifecycle
behavior with the operator-managed agent. Specialized Framework/CNI suites
retain their evidence-specific runtime deployments; never combine them with
an enabled operator-managed agent in the same lab. No production or
multi-chassis acceptance is implied. Remaining work includes durable OVN
storage/recovery, authentication/authorization/network isolation, narrower
namespace-scoped privilege boundaries, reproducible pinned images,
multi-node scheduling and upgrade/failure/convergence testing, runtime
configuration distribution, observability and standalone-provider design.
