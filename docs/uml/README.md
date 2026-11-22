# Architecture diagrams

PlantUML sources (`.puml`) are canonical and are kept beside generated SVG
previews. Render selected diagrams locally from the repository root with:

```sh
plantuml -tsvg docs/uml/component-overview.puml \
  docs/uml/evpn-runtime-architecture.puml \
  docs/uml/evpn-modular-architecture.puml \
  docs/uml/evpn-dataplane-comparison.puml \
  docs/uml/netlink-programmed-type2.puml
```

The component overview uses Graphviz inside PlantUML. Regenerate its final
SVG with:

```sh
python3 docs/uml/render-component-overview.py
```

The helper renders the canonical source, then adjusts only horizontal
coordinates: it centers the upper/lower annotations on three horizontal
arrows, aligns the BGP cluster's right edge with Neutron, shifts the framework
by the same amount, and centers the legend. Connected arrow endpoints follow
the shifted nodes; vertical coordinates are preserved. The helper also trims
unused horizontal canvas to the standard Graphviz padding without moving
diagram elements. Rendering with PlantUML alone does not apply these final
corrections. Python 3, PlantUML and
Graphviz (`dot` with JSON output support) are required.

## Maintained overview diagrams

- [Component overview](component-overview.svg) — logical responsibilities.
- [Runtime architecture](evpn-runtime-architecture.svg) — Kubernetes and
  node-local components.
- [Modular architecture](evpn-modular-architecture.svg) — framework modules.
- [Integration comparison](evpn-dataplane-comparison.svg) — control-plane,
  integration and workload-forwarding boundaries.
- [Netlink Type-2 path](netlink-programmed-type2.svg) — selected candidate
  integration, not a standalone workload dataplane.

These are conceptual diagrams, not deployment manifests or validation
results. GoBGP is the BGP/EVPN control plane; OVN/OVS forwards workload
traffic. The Linux bridge and Netlink-managed state are consumed by OVN/OVS.
Direct OVSDB programming is maintenance/test-only for the candidate.

## Scenario diagrams

The [Testing Guide](../../TESTING_GUIDE.md#live-test-scenarios-and-expected-behavior)
maps all nine scenarios to diagrams or explicit textual gaps. The
[scenario notes](../testing/lifecycle-scenarios.md) link actual harnesses,
procedures and qualification limits. The [user-facing guides](../use-cases/README.md) describe
[external-fabric integration](../use-cases/README.md#integrating-ovn-with-an-external-evpnvxlan-fabric)
as experimental and
[independent OVN-domain interconnection](../roadmap/README.md#independent-ovn-domain-interconnection-planned)
as planned. These diagrams explain the target topology and packet/control
flows; they are not test results.

The validated standalone two-chassis Type-2 scenario has no dedicated diagram
in this index. Do not treat the inventory-gated [Attempt 8](operator-inventory-type2-two-worker.svg)
topology as a depiction of that scenario or its acceptance.

| Diagram | Type and scope |
|---|---|
| [CNI lifecycle](evpn-cni-lifecycle-sequence.svg) | Sequence; CNI plus a separate optional external Type-2 test, not Geneve-only topology. |
| [Framework Netlink lifecycle](evpn-framework-netlink-e2e-sequence.svg) | Sequence; one-chassis non-CNI test contract. |
| [Shared-OVN secondary networking](multinode-secondary-external-domains.svg) | Topology; solid Geneve baseline, dashed external fixtures not a combined validation. |
| [Agent restart](node-agent-restart-sequence.svg) | Sequence; same-speaker durable recovery contract, not live evidence. |
| [Speaker restart](gobgp-speaker-restart-sequence.svg) | Sequence; attested replacement and fresh UUID contract, not live evidence. |
| [Orphan/API recovery](cni-orphan-api-recovery-sequence.svg) | Sequence; implemented CNI contract, operational qualification incomplete. |
| [Attempt 8](operator-inventory-type2-two-worker.svg) | Historical two-worker topology; no lifecycle acceptance. |
| [GoBGP/FRR](multidomain-gobgp-frr-type2.svg) | Historical one-chassis topology; not v0.1.0 acceptance. |
| [External EVPN/VXLAN fabric](external-evpn-fabric.svg) | Reference topology showing NB/northd/SB/controller/OVS, Node-Agent GoBGP API and Netlink boundaries, and a separate bounded historical packet path. |
| [External route and packet flow](external-evpn-type2-exchange.svg) | GoBGP AddPath/RIB and validated Netlink import are distinct from the bounded historical OVN/OVS VXLAN packet path. |
| [Independent OVN domains](multidomain-gobgp-gobgp-type2.svg) | Connected, planned topology with separate NB/SB control planes, local speakers and VTEPs; inter-domain route and packet paths are unvalidated. |
| [Independent-domain route and packet flow](multidomain-type2-flow.svg) | Proposed route import/mapping and VXLAN stages; tunnel/forwarding integration remains open and is not end-to-end validated. |
| [External withdrawal](external-evpn-withdrawal.svg) | Proposed lifecycle sequence; not release acceptance. |

Render the four scenario diagrams with:

```sh
plantuml -tsvg docs/uml/external-evpn-fabric.puml \
  docs/uml/external-evpn-type2-exchange.puml \
  docs/uml/multidomain-gobgp-gobgp-type2.puml \
  docs/uml/multidomain-type2-flow.puml
```

Render the focused lifecycle sequences with:

```sh
plantuml -tsvg docs/uml/node-agent-restart-sequence.puml \
  docs/uml/gobgp-speaker-restart-sequence.puml \
  docs/uml/cni-orphan-api-recovery-sequence.puml
```
