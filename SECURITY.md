# Experimental deployment security

This project is an experimental networking Framework, not a production-qualified
network platform. Use only a dedicated disposable Linux lab host, trusted peers
and an isolated management network. Do not deploy the examples in a shared
cluster or on a machine carrying production workloads or credentials.

## Trust boundaries

| Interface / component | Current boundary and required containment |
|---|---|
| evpn-mgmt TCP `:50052` | No TLS, authentication or authorization. Mutable speaker, peer and compatibility route operations. The checked-in kind/core manifests publish NodePort `30052`; firewall it from every untrusted network before applying them. |
| ovsdb-mgmt TCP `:50053` | Same transport limitations; checked-in kind/core manifests publish NodePort `30053`. Firewall it from every untrusted network. Can mutate OVN and OVS; even `GetVtep` can repair OVS external IDs. |
| GoBGP / OVN / OVSDB | Administrative control-plane interfaces. Accepted managed GoBGP tests use a node-local Unix socket. Do not expose unauthenticated TCP listeners outside the dedicated lab. |
| NodeAgentCNI | Unix socket mode `0600`; filesystem permissions and trusted node processes are the boundary. The agent can change endpoint/network state and enter workload namespaces. |
| Kubernetes / Operator | Examples use elevated permissions, privileged containers, host networking and host paths where needed. EvpnCore RBAC is broad and readiness is apply-only, not a security or operational health check. |
| Containerlab / Docker | Docker and lab provisioning permissions are effectively host administration. Run only trusted source and pinned runtime inputs on a disposable host. |

Containerlab management PKI is separate from application transports.
**Framework gRPC TLS was not enabled in the historical C4.4f.2q.2 acceptance
runs.** No authenticated Framework management acceptance is claimed.
The [API reference](docs/api/README.md) lists implemented, unimplemented,
compatibility-oriented and state-mutating operations.

Do not treat ClusterIP, a container network, or a Unix socket alone as a complete
multi-tenant security design. Enforce host/network access restrictions outside
the example deployment. If that isolation cannot be guaranteed, do not run it.
Before applying `deploy/kind/k8s.yaml` or the Operator core manifest, enforce
host/perimeter firewall rules that deny untrusted access to **every node's**
NodePorts `30052` and `30053`, and to any exposed OVN/OVSDB/GoBGP endpoints.
Restrict pod-to-service reachability to trusted lab workloads as well; host
firewall rules alone do not provide Kubernetes tenant isolation. Verify denial
from an untrusted segment and intended access from the trusted lab segment.
Do not attach this lab to a public or shared network. If these conditions cannot
be enforced, do not apply the examples. ClusterIP and loopback-only port-forwarding
would reduce exposure but do not add authentication.

The maintainer explicitly accepted the HIGH unauthenticated NodePort risk for
this experimental source release **only under this dedicated-lab containment**.
The MEDIUM broad Operator RBAC risk includes cluster-wide Secret read/write/delete
permission: never install this Operator in a cluster holding unrelated secrets.
These are documented residual risks, not remediated findings or validated
production controls. No runtime hardening was performed in preparation.
Network authentication, authorization and narrower deployment privileges remain
[API-02 / K8S-01 roadmap work](docs/roadmap/backlog.md).

## Credentials and generated material

Generate fresh lab certificates locally with
[`tools/generate-lab-certs.sh`](tools/generate-lab-certs.sh). Keep its output,
kubeconfigs, keys, environment files, raw logs, packet captures and acceptance
bundles outside source control and public artifacts. Never reuse historical lab
CA material. Review evidence before sharing: captures and diagnostics can contain
workload addresses, identities or credentials.

## Cleanup and reproduction

Read the [Containerlab ownership/recovery procedure](deploy/containerlab/README.md)
before running a live suite. Use only exact run-owned topology receipts for
cleanup. Never use global destroy, Docker prune, wildcard resource deletion, or
an existing cluster as a test environment. Offline checks in the
[Quick Start](QUICK_START.md) do not require privileged live tests.

## Reporting

Do not put credentials, private keys or sensitive captures in public issues.
For a suspected vulnerability, first request a private reporting channel from
the maintainer without disclosing exploit details or sensitive material.
No response-time commitment or independent security certification is implied.
