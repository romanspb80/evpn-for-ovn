# Architecture diagrams

These editable PlantUML sources describe one reusable EVPN Framework, its
logical responsibilities, Kubernetes runtime, alternative integration modes,
evidence-bounded topologies and selected E2E flows. The component overview is
a product-architecture view, not a feature-availability or validation claim.
Netlink-Programmed is the
primary integration target; OVSDB-Programmed is experimental. OVN/OVS handles
workload forwarding in both. The
Netlink kernel state is an integration boundary under the upstream
[OVN Dynamic Routing EVPN architecture](https://docs.ovn.org/en/latest/topics/dynamic-routing/architecture.html#evpn-ethernet-vpn-integration),
not a standalone workload forwarding dataplane.
The [component overview](component-overview.svg) is the compact product
architecture entry diagram used by the [root README](../../README.md); its
external-peer label does not claim interoperability with every vendor. The
modular ownership, dataplane
comparison and runtime diagrams add detail in their respective architecture
pages; this catalog is not a substitute for those explanations.
The [Kubernetes runtime](evpn-runtime-architecture.svg) is a logical deployment
view, not an E2E acceptance diagram. The
[attempt-8 topology](operator-inventory-type2-two-worker.svg) records only its
scoped two-worker forwarding result.
The three older external-fabric design diagrams are labeled **PLANNED / NOT
EXECUTED** and do not themselves
extend accepted E2E coverage. Their [acceptance plan](../testing/external-evpn-fabric.md)
documents local renderer versions, exact regeneration commands and limits.

| Diagram | Purpose and documentation | Editable source | Render |
|---|---|---|---|
| Component overview | Product architecture: one Framework, consumers, integration backends, GoBGP and OVN/OVS; [root README](../../README.md) | [`.puml`](component-overview.puml) | [SVG](component-overview.svg) |
| Modular architecture | Implemented logical responsibilities, ownership and recovery boundaries; [framework architecture](../architecture/framework-architecture.md) | [`.puml`](evpn-modular-architecture.puml) | [SVG](evpn-modular-architecture.svg) |
| Kubernetes runtime | Operator, per-node Node-Agent, optional provider, CNI and separate datapaths; [deployment guide](../architecture/deployment.md) | [`.puml`](evpn-runtime-architecture.puml) | [SVG](evpn-runtime-architecture.svg) |
| Integration-mode comparison | Netlink-Programmed primary target versus experimental OVSDB-Programmed backend; [integration modes](../architecture/integrations.md) | [`.puml`](evpn-dataplane-comparison.puml) | [SVG](evpn-dataplane-comparison.svg) |
| Shared OVN Geneve / external fixtures | Validated shared-OVN CNI baseline versus external fixtures tested separately; [topology guide](../architecture/network-topologies.md#shared-ovn-geneve-with-external-domains) | [`.puml`](multinode-secondary-external-domains.puml) | [SVG](multinode-secondary-external-domains.svg) |
| Inventory-gated two-worker Type-2 | Attempt-8 only: accepted inventory, independently attested speakers, external FRR forwarding; [acceptance record](../architecture/operator-type2-acceptance.md#attempt-8-inventory-gated-two-worker-type-2-forwarding) | [`.puml`](operator-inventory-type2-two-worker.puml) | [SVG](operator-inventory-type2-two-worker.svg) |
| GoBGP / FRR Type-2 | Bounded one-chassis interoperability; not two OVN domains; [topology guide](../architecture/network-topologies.md#gobgp-and-frr-type-2-domains) | [`.puml`](multidomain-gobgp-frr-type2.puml) | [SVG](multidomain-gobgp-frr-type2.svg) |
| GoBGP / GoBGP independent OVN domains | Proposed, unvalidated two-domain topology; [topology guide](../architecture/network-topologies.md#gobgp-and-gobgp-independent-ovn-domains) | [`.puml`](multidomain-gobgp-gobgp-type2.puml) | [SVG](multidomain-gobgp-gobgp-type2.svg) |
| Framework Netlink E2E sequence | One-chassis route import/export, forwarding and cleanup; [Testing Guide](../../TESTING_GUIDE.md) | [`.puml`](evpn-framework-netlink-e2e-sequence.puml) | [SVG](evpn-framework-netlink-e2e-sequence.svg) |
| Framework OVSDB E2E sequence | Experimental opt-in route import, programming, withdrawal and cleanup; [Testing Guide](../../TESTING_GUIDE.md) | [`.puml`](evpn-framework-ovsdb-e2e-sequence.puml) | [SVG](evpn-framework-ovsdb-e2e-sequence.svg) |
| Kubernetes CNI lifecycle sequence | CNI ADD/CHECK/DEL and asynchronous Type-2 lifecycle; [CNI solution](../solutions/kubernetes-cni/README.md) | [`.puml`](evpn-cni-lifecycle-sequence.puml) | [SVG](evpn-cni-lifecycle-sequence.svg) |
| External EVPN fabric proposal | Historical multi-worker design, **PLANNED / NOT EXECUTED**; [fabric record](../testing/external-evpn-fabric.md) | [`.puml`](external-evpn-fabric.puml) | [SVG](external-evpn-fabric.svg) |
| External Type-2 exchange proposal | Proposed policy, ownership and route-exchange flow; [fabric record](../testing/external-evpn-fabric.md) | [`.puml`](external-evpn-type2-exchange.puml) | [SVG](external-evpn-type2-exchange.svg) |
| External withdrawal proposal | Proposed exact-UUID withdrawal and fail-closed flow; [fabric record](../testing/external-evpn-fabric.md) | [`.puml`](external-evpn-withdrawal.puml) | [SVG](external-evpn-withdrawal.svg) |

Render SVGs from the repository root with PlantUML and Graphviz:

```bash
plantuml -tsvg docs/uml/*.puml
```

The `.puml` files are canonical; never edit generated SVG layout or text
independently. The SVGs are direct PlantUML output; there is no canvas
post-processing step. No external rendering service is required. Blue boxes
represent project core, amber project integration and slate third-party solutions.
PLANNED labels and dashed arrows distinguish future adapters; blue EVPN-control
arrows and green packet arrows are not interchangeable.
Click an embedded preview to open its full-size SVG.

## Semantic source matrix

| Diagram family | Implementation grounding |
|---|---|
| Component overview / comparison | `pkg/evpn`, `pkg/endpoint`, `pkg/attachment`; [EVPN Framework Architecture](../architecture/framework-architecture.md). k8gobgp is optional outside Kubernetes, not an EVPN Core dependency. |
| Modular architecture | [Verified component matrix and boundary status](../architecture/framework-architecture.md): EVPN Core is a logical classification, not a package/service split. Node-Agent hosts reusable logic but current authority/recovery is Kubernetes/containerd-specific. |
| Kubernetes runtime | `deploy/kind/k8s.yaml`, `deploy/operator/core-resources.yaml`, accepted runner configuration and [upstream runtime inventory](../architecture/runtime-dependencies.md). A declared TCP Service is not an installed proxy. |
| Netlink E2E | `tools/run-framework-netlink-e2e.sh`, `pkg/nodeagent/framework_netlink_e2e_test.go`, `pkg/evpn/local_netlink.go`, `remote_netlink_import.go`, `type2_gobgp.go`. |
| OVSDB E2E | `tools/run-framework-ovsdb-e2e.sh`, `cmd/evpn-ovsdb`, `pkg/evpn/ovsdb_programmer.go`, `ovsdb_provider_bridge.go`. Cross-database operations and patch cleanup are asynchronous/non-atomic. |
| CNI lifecycle | `pkg/nodeagent/server.go`, `pkg/endpoint/manager.go`, `tools/lib/cni-evpn-frr-interop-test.sh`. Type-2 export is asynchronous; test network deletion is guarded runner-side NB CLI, not a node-agent RPC. |
| External fabric | [Historical fixture and design](../testing/external-evpn-fabric.md): shared OVN, two workers, separate FRR/Linux and GoBGP/Linux peers; Remote2 uses a test-only RIB projector. Individual scenarios have bounded evidence; full acceptance remains unresolved. |

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
network/remote VTEP. The Kubernetes CNI solution has a separate acceptance
scope. The operator-managed recovery record is linked separately from the older
successful-flow sequences; the diagrams are not an exhaustive acceptance matrix.
No diagram alone establishes generalized shared-VTEP, two-chassis recovery,
RR, Type-5 or production readiness. See the
[capability matrix](../project/capability-matrix.md) for bounded test scope.
Containerlab participants are test-only.

## Preserved drafts and historical views

`drafts/component-overview-draft.puml` and
`drafts/kubernetes-runtime-draft.puml` are retained as historical design
iterations; the canonical overview and Kubernetes runtime above supersede
them. The earlier runtime diagram was a mixed lab/compatibility wiring view;
its filename is retained for existing links, but its source and SVG now show
the canonical logical Kubernetes runtime. The draft files are not regenerated
or treated as acceptance evidence.
