# Containerlab E2E laboratories

**Containerlab is the standard and required environment provisioning mechanism
for all EVPN-for-OVN E2E tests.** It is not a Framework runtime, build, unit-test,
or production dependency.

Read [SECURITY.md](../../SECURITY.md) first. The kind/core application examples
publish unauthenticated management NodePorts `30052`/`30053`; isolate and firewall
every lab node before applying them. Containerlab TLS does not secure these RPCs.

## Prerequisites

Use a dedicated disposable **Linux amd64** host with Docker Engine, kind,
kubectl, Python 3.11+, OpenSSL, Go (see `go.mod`), `flock`, and
**Containerlab 0.79.0**. Install Containerlab using its
[official installation instructions](https://containerlab.dev/install/).
This exact release is currently enforced because native kind's configuration
schema differs between versions. Do not silently use newer documentation with
an older executable. Docker access and Containerlab privileges (`clab_admins`
or equivalent approved installation) are required. The CNI capture helpers
also require tcpdump, systemd and passwordless access to their documented
privileged capture commands. macOS, Windows, rootless Docker and other
Containerlab versions have not been accepted.

The accepted host used Docker Engine **29.1.2** and kind CLI **v0.27.0**.
The native kind topology pins the Kubernetes **v1.32.3** node image by digest;
host kind CLI version and Kubernetes node version are different identities.

Required pinned images and runtime identities are in
[runtime dependencies](../../docs/architecture/runtime-dependencies.md).
The runners refuse mismatched local OVN/base image IDs. Internet access may be
needed for pinned images, the checksum-verified k8gobgp manifest, Multus, and
Go modules. Containerlab does not install these host tools.

## One provisioning model

[`tools/e2e/containerlab.py`](../../tools/e2e/containerlab.py) renders a
run-specific `lab.clab.yaml` and immutable-topology ownership `lab.json` in
the evidence directory. Four profiles share this implementation:

| Profile | Containerlab environment | Suite-specific setup |
|---|---|---|
| `framework-netlink` | Native `k8s-kind`, two nodes, pinned Linux FRR peer and underlay | OVN/OVS, managed k8gobgp, Framework services, endpoints and assertions |
| `framework-ovsdb` | Same common kind/FRR topology | Explicit OVSDB importer, provider bridge/localnet, endpoints and assertions |
| `cni` | Same native kind topology; FRR included in interop modes | Multus, node-agent, CNI workloads and selected lifecycle/continuity assertions |
| `type2` | Linux standalone gobgpd and FRR on a run-owned internal bridge | BGP readiness, Type-2 route and isolated endpoint lifecycle; **not managed k8gobgp coverage** |

[`topologies/kind.yaml`](topologies/kind.yaml) is the common native kind
configuration. Suite selection supplies names and optional FRR; there are no
three divergent copies of a laboratory. The former static topology and
independently orchestrated Compose fixture are retired.

Containerlab's native `k8s-kind` provider creates/deletes the cluster; the
coordinator exports its kubeconfig as the invoking user into the private
evidence directory. The shared Docker `kind` network is not owned or deleted.
The coordinator attaches only its worker to the FRR management/underlay bridge.
Containerlab 0.79 cannot create an `Internal=true` management network, so the
Type-2 coordinator reserves an explicitly labeled internal Docker bridge
before Containerlab deploys its Linux nodes. Containerlab teardown and exact
ID checks manage that bridge's lifecycle. This is coordinated ownership,
not a claim that native Containerlab creates the internal bridge.

The suite deploys Kubernetes resources only after topology and certificate
readiness. Actual manager/gobgpd, BGP/EVPN, route and workload readiness checks
remain in the suite; environment readiness alone is not acceptance.

## Running and collecting evidence

Run individual suites from the repository root:

```bash
bash tools/run-framework-netlink-e2e.sh
bash tools/run-framework-ovsdb-e2e.sh
bash tools/run-cni-full-lifecycle-e2e.sh
```

The same lifecycle is used by every mode of
`tools/run-cni-mvp-2.4b-multus.sh`. The standalone fixture's commands are
documented [here](../../tests/e2e/type2-fixture/README.md).
Run the primary acceptance matrix **sequentially**, twice:

```bash
for run in 1 2; do
  bash tools/run-framework-netlink-e2e.sh || exit "$?"
  bash tools/run-framework-ovsdb-e2e.sh || exit "$?"
  bash tools/run-cni-full-lifecycle-e2e.sh || exit "$?"
done
```

Each runner prints its unique evidence directory. It contains topology,
Containerlab version/deploy/destroy logs, resource IDs, CA verification, and
the existing runtime/route/packet/lifecycle evidence. Never publish raw
kubeconfigs or `.tls` keys from these directories.

For manual **environment-only** bootstrap (no Framework deployment/assertions):

```bash
python3 tools/e2e/containerlab.py up --directory /tmp/evpn-manual-lab \
  --profile framework-netlink --cluster manual-evpn \
  --network evpn33b3-manual-evpn --frr manual-evpn-frr
python3 tools/e2e/containerlab.py status --directory /tmp/evpn-manual-lab
python3 tools/e2e/containerlab.py collect --directory /tmp/evpn-manual-lab
python3 tools/e2e/containerlab.py down --directory /tmp/evpn-manual-lab
```

`status`/`collect` validate ownership, save bounded container logs and inventory,
and report current resources. `down` is idempotent and uses only the exact
reserved topology. `deploy-kind-frr.sh [up|status|collect|down]` is a compatibility
wrapper for this interface, not the former destructive deployment installer.
After `down`, use a **new evidence directory** for another run.

## Ownership, isolation and failure recovery

The entire primary suite and each standalone fixture/component action hold a
shared exclusive `flock`; a second run fails
explicitly. A persistent lease also prevents retained or failed laboratories
from overlapping another suite. Fixed benchmark subnets/VNIs and global
before/after inventory assertions make parallel acceptance unsupported.
No fixed workload/gRPC host ports are published. Native kind allocates a
loopback host port for the Kubernetes API.

Preflight refuses existing target containers/networks and overlapping subnets.
Linux nodes carry lab/run labels; native kind containers carry native cluster
labels. Containerlab management networks have only a generic marker, so their
ownership is bound by an absent-before reservation, exact name/subnet and saved
Docker ID, **not an invented per-lab Containerlab network label**. Teardown
checks IDs, labels, kind membership and attached network members before invoking
`containerlab destroy -t <exact-topology>`. Unknown members or replacements
block deletion. There is no `--all`, wildcard deletion or Docker prune.

Failed deploys retain partial resource receipts and diagnostics. Runners always
attempt scoped teardown, even if cluster creation did not complete. For an
interrupted/retained run, first ensure its process is no longer active, then run
`collect` and `down` with **that run's directory**. Do not delete the manifest,
edit its topology, release its lease, or use global cleanup to bypass ownership
checks. A hard kill before a resource receipt is saved can require explicit
ownership review; ambiguous resources are preserved, never adopted by name.
Containerlab's scoped `/etc/hosts`, SSH configuration and network-namespace
symlinks are removed through its matching destroy operation.

## Management PKI

Bootstrap reuses [`tools/generate-lab-certs.sh`](../../tools/generate-lab-certs.sh).
Persistent trust lives in
`~/.local/state/evpn-for-ovn/containerlab/tls`, outside source, Docker build
contexts and disposable lab cleanup. It generates fresh local CA/server/client
material, validates SAN/EKU/key pairing, rejects invalid/compromised material,
and reuses valid bundles without rotation. Keys are mode `0600`; the directory
is `0700`. Stop dependent labs before explicit rotation:

```bash
bash tools/generate-lab-certs.sh \
  --output-dir "$HOME/.local/state/evpn-for-ovn/containerlab/tls" --regenerate
```

Bootstrap exports `CLAB_CA_CERT_FILE`/`CLAB_CA_KEY_FILE`. Linux topology nodes
request certificates from Containerlab; bootstrap verifies their actual issued
leaf chains and hostname SANs against the intended CA and records its public
fingerprint. External CA files are **not copied** into Containerlab's empty
lab-local `ca` directory. A kind-only CNI profile records `NOT_USED` for
Containerlab node TLS; kind manages Kubernetes PKI independently.
These management certificates do **not** enable Framework gRPC TLS.

See the [Testing Guide](../../TESTING_GUIDE.md) for accepted scopes, evidence,
discovery/classification and limitations.
