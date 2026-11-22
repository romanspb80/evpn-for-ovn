# Experimental deployment security

EVPN for OVN is experimental and is not qualified for production or shared
clusters. Use examples only in a dedicated disposable Linux environment with
trusted operators, peers and an isolated management network. Do not expose
management APIs, OVN databases or privileged workloads to untrusted networks.

## Trust boundaries

- **CNI peer authentication:** in real OVN/OVS plus Linux-veth mode, the
  Node-Agent authenticates the connected Unix peer using root:root
  `SO_PEERCRED` and kernel `SO_PEERPIDFD`. This is mandatory even when optional
  continuity is disabled. Socket permissions are an additional restriction,
  not a substitute for peer identity. A compromised host or root process is
  outside this protection boundary.
- **Continuity and endpoint authorization:** continuity adds caller/process,
  CRI workload and attachment checks. Standalone export requires protected
  administrator policy, durable fencing, OVN identity checks and explicit
  GoBGP speaker attestation. Do not infer authorization from a route, socket,
  health check or OVN external IDs alone.
- **Control plane and forwarding:** GoBGP handles BGP/EVPN control-plane
  state. OVN/OVS forwards workload packets. Linux Netlink manages state used by
  OVN/OVS; Linux bridges are not an independent workload dataplane.
- **Legacy management RPCs:** `evpn-mgmt` and `ovsdb-mgmt` use gRPC without
  transport authentication or authorization. The legacy Kind compatibility
  profile limits these services to ClusterIP, but any permitted in-cluster
  caller remains unauthenticated. ClusterIP is not authentication or
  multi-tenant isolation. Do not deploy this compatibility profile in a
  shared or untrusted cluster.
- **Supported Operator profile:** its Netlink core profile excludes the legacy
  management services and `bgp_ad_handler`. The Operator and Node-Agent remain
  privileged, with broad permissions and host access; constrain who can
  create or modify their configuration and run them only on trusted nodes.
- **External BGP peers:** authentication and filtering of remote BGP sessions
  are separate from local speaker attestation and endpoint authorization.
  TCP MD5 is not encryption. Use independent network controls and reviewed
  peer policy; interoperability and production key rotation are not claimed.
- **Host and test tooling:** Docker, Containerlab, Kubernetes administration,
  host mounts and privileged containers effectively grant host-level control.
  Run only reviewed source and pinned inputs on a disposable host.

The benchmark-only Netlink provenance bypass is not part of the supported
secure profile and must not be enabled for release or operational use.

## Configuration and evidence handling

Keep kubeconfigs, credentials, private keys, certificates, environment files,
raw logs, packet captures and acceptance bundles out of source control and
public artifacts. Generate fresh lab credentials locally; never reuse lab CA
material. Captures and diagnostics can expose workload addresses and identities.

Use the run-owned cleanup procedure in the [Containerlab guide](deploy/containerlab/README.md).
Never use global destroy, Docker prune, wildcard resource deletion, or an
existing cluster as a test environment. Offline tests do not require
privileged live infrastructure.

## Reporting

Do not include credentials, private keys or sensitive captures in public
issues. Contact the maintainer through a private channel for suspected
vulnerabilities; no response-time commitment or independent security
certification is implied.
