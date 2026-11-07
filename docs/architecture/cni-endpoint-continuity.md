# Durable CNI endpoint continuity

**Creation-time acceptance now passes with authenticated two-stage admission.**
Real containerd v2.0.3 Pods reach verified activation, exchange Type-2 routes
and survive Node-Agent restart. Full speaker-replacement convergence remains
incomplete. See [creation authority and evidence](cni-creation-authority.md).

Enable the bounded node-local layer with:

```text
--mode=real-ovn-ovs --network-backend=linux-veth \
--cni-continuity-config=/etc/evpn/cni-continuity/config.json
```

Example administrator-authored configuration (replace all placeholders):

```json
{
  "JournalDirectory": "/var/lib/evpn-node-agent/cni",
  "Create": true,
  "Domain": {
    "InstallationUID": "<installation UUID>",
    "NodeUID": "<Kubernetes Node UID>",
    "BootID": "<host /proc/sys/kernel/random/boot_id>",
    "NetworkNamespace": "<host /proc/self/ns/net symlink value>"
  },
  "NodeName": "<Kubernetes node name>",
  "Kubeconfig": "",
  "CRIEndpoint": "unix:///run/containerd/containerd.sock",
  "Networks": {
    "net-h22": "ls-h22"
  },
  "CTRPath": "/usr/local/libexec/evpn-ctr",
  "CTRSHA256": "<static ctr binary SHA256>"
}
```

The configuration file and its ancestors must be root-owned and not writable
by group/others; the file must be regular, single-link and at most 64 KiB.
The journal's existing parent directories must satisfy `pkg/ownership` rules.
`Create` initializes only a newly created final directory. Missing metadata in
an existing directory is blocked even with `Create: true`.
`Domain.ConfigDigest` may be omitted: the agent calculates it from node name,
Kubernetes access path, CRI/OVN/OVS endpoints, complete network-to-switch map,
and optional runtime-inspector path/hash. If supplied, it must match.
Domain mismatch, including a changed host boot or network namespace, blocks.

An empty `Kubeconfig` selects in-cluster authentication. The service account
needs only `get` on Pods and Nodes; no Kubernetes writes are performed.
Multus need not provide `K8S_NODE_NAME`. When absent, ADD uses a private request
clone and binds that missing claim to configured `NodeName`, then independently
verifies the actual Pod placement and Node UID. An explicitly different node
claim is rejected, and missing Pod identity is never inferred from local state.
The agent needs host network and PID visibility, CNI namespace access, the
containerd socket, and root-private persistent state. A read-only volume mount
does **not** make the containerd Unix API read-only; possession of the socket is
a privileged trust boundary. The implementation invokes only status/read calls.
CRI connects eagerly through an actual-transport Unix dialer. Every connection
and reconnect requires root-owned, non-writable, no-symlink ancestors, a
single-link root-owned socket mode 0600, unchanged socket/parent device and inode
across dialing, and kernel `SO_PEERCRED` identifying a visible root:root peer.
Failure to establish that trusted connection blocks startup before the CNI socket.

`CTRPath` and `CTRSHA256` are optional as a pair. Initial CNI ADD can precede
containerd's publication of the CRI sandbox. This creation-only fallback reads
the administrator-pinned static executable by held file descriptor, requires
containerd sandbox metadata agreeing with the independently read Pod, and
checks the already running pause-task network namespace. It is not used to
replace failed Kubernetes observations, nor to relax recovery/CHECK readiness.

This fallback does **not** work with containerd v2.0.3's normal sandbox
creation ordering. In that version, `internal/cri/server/sandbox_run.go:242`
runs CNI setup before `CreateSandbox`/`StartSandbox` at lines 256/260;
`internal/cri/server/podsandbox/sandbox_run.go:195,244` creates the container
and task afterward. The namespace-creation thread has already restored its
original namespace and exited. Consequently there is no pause-task process
to attest during initial ADD, and `containers info <sandbox ID>` returns
NotFound. Waiting for that task from inside CNI would deadlock startup.
Pre-CNI sandbox metadata and a namespace path alone do not satisfy the
implemented process-association check. No bypass was introduced.

The [implemented two-stage creation authority](cni-creation-authority.md)
resolves that ordering without using the fallback: authenticated runtime/CNI
invocation and held namespace identity permit pending creation, while direct
READY sandbox checks remain mandatory before activation, export and CHECK.
Configurations without this admission authority retain the old limitation.

## Recovery and acknowledgements

The main process obtains authority before any socket preparation. CNI recovery
runs before the socket opens and serializes with ADD/CHECK/DEL. Shutdown prevents
queued operations from mutating after draining; journal/backend cleanup and
process-authority release occur afterward.

Recovery restores acknowledged records only after direct current Kubernetes
Node/Pod UID and node-placement checks, direct CRI READY sandbox metadata/PID
checks, exact OVN LSP/switch UUID and OVS Interface/Port ownership/membership,
and Linux veth peer/MAC/address/namespace checks. Names and static tuples are
not adoption evidence. Recorded host interface index and namespace device/inode
must also remain unchanged.

ADD durably writes its intent before mutation. Success is acknowledged only
after independent resource and workload verification and durable state commit.
Verified replay never recreates resources. Fresh ADD requires resource absence.
DEL writes intent, then uses exact UUID deletes with transactional ownership and
membership guards, and an acknowledged veth index. It commits a deleted ACK
only after independently observing resource absence. Partial/stale identities
never authorize cleanup.

Uncertain ADD/DEL results retain their pending intents. Restart and further
mutations fail closed: no automatic inference, rollback, or tuple-based adoption
resolves those outcomes. Administrator investigation must establish their
outcome; the Kubernetes operator does not resolve external CNI mutation intents.
A definitive Kubernetes Pod NotFound omits an active record from the live store
without recreating the Pod or automatically deleting residual resources. API
outage, UID replacement, deleting Pod, and missing runtime observations are not
treated as NotFound.

Exporter callers should gate desired native/CNI snapshots with
`ValidateRecoveredExportEndpoints`. Native `ovn-netlink/<LSP UUID>` IDs map to
the durable LSP receipt; native SB binding incarnation remains independently
verified by the native observer. Unknown or deleted-Pod native endpoints cannot
be exported from residual tuples. `ValidateContinuity` provides a whole-store
identity/resource gate, including empty desired snapshots. Disabled continuity
preserves the existing exporter behavior.
