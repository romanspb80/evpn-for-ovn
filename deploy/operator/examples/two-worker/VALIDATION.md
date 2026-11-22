# Two-worker Type-2 validation procedure

This document is a test procedure, not evidence that the example has passed.
Use only an explicitly authorized disposable cluster,
two dedicated workers and a dedicated external peer; never use a shared or
production cluster.

## Preconditions

- Follow [security guidance](../../../../SECURITY.md) and the
  [Operator guide](../../../../docs/architecture/evpn-core-operator.md).
- Replace all placeholders with reachable, non-overlapping values and verify
  the exact Kubernetes context before each operation.
- Establish distinct node, chassis, speaker, VTEP and journal identities.
- Confirm immutable image/build identities and the intended OVN NB/SB domain.
- Record run ownership and a scoped cleanup procedure before creating
  resources. Do not remove unowned routes, ports, devices or journals.

## Required observations

1. Verify the Operator configuration, worker placement, Node-Agent readiness,
   local OVN chassis identity, speaker process identity and current
   authorization/inventory state on each worker.
2. Verify each local endpoint's OVN logical switch port and ready Southbound
   binding, then confirm the expected local Type-2 path and exact owned route
   receipt at the speaker and external peer.
3. Verify remote Type-2 policy/mapping and owned Linux FDB/neighbor state on
   each worker. Incomplete RIB snapshots or pending intents block the test.
4. Exercise both directions between each workload and the external endpoint.
   Preserve packet captures from the verified underlay and correlate them to
   the expected VTEPs, VXLAN VNI and workload addresses. Do not infer traffic
   from BGP, ping output or an FDB entry alone.
5. Test exact endpoint withdrawal and survivor/foreign-state preservation.
   Record before/after route UUIDs and resource ownership. Stop if state is
   ambiguous or an unrelated object changes.
6. Run agent or speaker restart cases separately, repeating identity, route,
   attachment and packet checks after recovery. These cases do not establish
   missed-DEL/orphan cleanup or Kubernetes API-outage behavior.
7. Clean only resources owned by the test and verify cleanup. Retain all
   evidence needed to reproduce the result.

Record each outcome as PASS, FAIL or BLOCKED with the exact source, image,
environment, assertion and cleanup status. The example is not release or
production evidence until the complete procedure has been executed and
independently reviewed.
