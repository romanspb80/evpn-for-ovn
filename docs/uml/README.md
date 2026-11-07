# Architecture diagrams

These seven editable PlantUML sources describe the current Framework, its
logical modules, deployment, two integration modes, and three accepted E2E
test flows. Optional and future components are labeled. OVN/OVS handles
workload forwarding. The
Netlink kernel state is an integration boundary under the upstream
[OVN Dynamic Routing EVPN architecture](https://docs.ovn.org/en/latest/topics/dynamic-routing/architecture.html#evpn-ethernet-vpn-integration),
not a standalone workload forwarding dataplane.
The [platform overview](evpn-overview.svg) and
[modular ownership diagram](evpn-modular-architecture.svg) are the two primary
diagrams in the [root README](../../README.md).

| Diagram | Editable source | Render |
|---|---|---|
| Framework overview and application boundaries | [`.puml`](evpn-overview.puml) | [SVG](evpn-overview.svg) |
| Modular architecture and component ownership | [`.puml`](evpn-modular-architecture.puml) | [SVG](evpn-modular-architecture.svg) |
| Integration-mode comparison | [`.puml`](evpn-dataplane-comparison.puml) | [SVG](evpn-dataplane-comparison.svg) |
| Kind deployment | [`.puml`](evpn-runtime-architecture.puml) | [SVG](evpn-runtime-architecture.svg) |
| Independent Netlink Framework E2E | [`.puml`](evpn-framework-netlink-e2e-sequence.puml) | [SVG](evpn-framework-netlink-e2e-sequence.svg) |
| Independent OVSDB Framework E2E | [`.puml`](evpn-framework-ovsdb-e2e-sequence.puml) | [SVG](evpn-framework-ovsdb-e2e-sequence.svg) |
| CNI Full Lifecycle E2E | [`.puml`](evpn-cni-lifecycle-sequence.puml) | [SVG](evpn-cni-lifecycle-sequence.svg) |

Render SVGs from the repository root with:

```bash
plantuml -tsvg docs/uml/*.puml
python3 - <<'PY'
from pathlib import Path

# PlantUML 1.2020.02 emits the canvas as CSS; some SVG image renderers ignore it.
# Keep the two primary diagrams opaque without changing their generated layout.
canvas = '<rect x="0" y="0" width="100%" height="100%" fill="#FCFCFD"/>'
for name in ("evpn-overview", "evpn-modular-architecture"):
    path = Path("docs/uml") / (name + ".svg")
    svg = path.read_text()
    if canvas not in svg:
        assert "<defs/>" in svg, "Unexpected PlantUML SVG structure"
        path.write_text(svg.replace("<defs/>", "<defs/>" + canvas, 1))
PY
```

PlantUML sources and the deterministic canvas step above are canonical;
never edit generated layout/text independently.
The original six sources and paired SVGs were audited/rendered locally for C4.4f.2r
with PlantUML 1.2020.02 and Graphviz 2.43.0. No external rendering service is
required. The two primary diagrams use native PlantUML syntax and a fixed near-white
canvas with dark text, so labels remain readable on GitHub light and dark page
backgrounds. Blue boxes represent project core, amber project integration and
slate third-party solutions. PLANNED labels and dashed arrows distinguish future
adapters; blue EVPN-control arrows and green packet arrows are not interchangeable.
Click either README image for the full-size SVG.

## Semantic source matrix

| Diagram family | Implementation grounding |
|---|---|
| Overview / comparison | `pkg/evpn`, `pkg/endpoint`, `pkg/attachment`; [Framework boundaries](../architecture/framework-first.md). k8gobgp is optional outside Kubernetes, not a Framework dependency. |
| Modular architecture | [Verified component matrix and boundary status](../architecture/framework-first.md): core is a logical classification, not a package/service split. Node-Agent hosts reusable logic but current authority/recovery is Kubernetes/containerd-specific. |
| Kubernetes runtime | `deploy/kind/k8s.yaml`, `deploy/operator/core-resources.yaml`, accepted runner configuration and [upstream runtime inventory](../architecture/runtime-dependencies.md). A declared TCP Service is not an installed proxy. |
| Netlink E2E | `tools/run-framework-netlink-e2e.sh`, `pkg/nodeagent/framework_netlink_e2e_test.go`, `pkg/evpn/local_netlink.go`, `remote_netlink_import.go`, `type2_gobgp.go`. |
| OVSDB E2E | `tools/run-framework-ovsdb-e2e.sh`, `cmd/evpn-ovsdb`, `pkg/evpn/ovsdb_programmer.go`, `ovsdb_provider_bridge.go`. Cross-database operations and patch cleanup are asynchronous/non-atomic. |
| CNI lifecycle | `pkg/nodeagent/server.go`, `pkg/endpoint/manager.go`, `tools/lib/cni-evpn-frr-interop-test.sh`. Type-2 export is asynchronous; test network deletion is guarded runner-side NB CLI, not a node-agent RPC. |

The high-level diagrams explicitly separate BGPConfiguration,
k8gobgp manager reconciliation and managed gobgpd; Framework gRPC owns
Type-2 paths rather than managed BGP configuration. Optional EvpnCore
manages YAML-driven workload lifecycle (including Node-Agent), not upstream
BGPConfiguration/DaemonSet. Future CRDs are proposals only.

Sequences are source-grounded successful-flow/exception summaries, not
instruction-by-instruction traces or total ordering of asynchronous loops.
Northd produces SB logical state; Framework managers attach local interfaces;
ovn-controller claims bindings and programs forwarding. CNI responses do not
wait for BGP convergence. The failure inset distinguishes accepted pre-LSP
rollback from unimplemented general post-LSP compensation.

Independent Framework tests retain their one-chassis limits; OVSDB has one
network/remote VTEP. CNI is a separate application acceptance scope. The
operator-managed recovery record is linked separately from the older
successful-flow sequences; the diagrams are not an exhaustive acceptance matrix.
No diagram establishes shared-VTEP, two-chassis, RR, Type-5 or production
readiness. Containerlab participants are test-only.
