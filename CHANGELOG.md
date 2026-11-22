# Changelog

This changelog summarizes the experimental v0.1.0 scope.

## v0.1.0 Experimental

- Selects the Netlink-programmed EVPN integration for the candidate profile.
- Keeps GoBGP as the BGP/EVPN control plane and OVN/OVS as the workload
  forwarding dataplane.
- Requires root:root Unix peer authentication for the real OVN/OVS plus
  Linux-veth CNI transport, independent of optional continuity checks.
- Provides isolated lab scenarios for secondary-CNI, external Type-2/VXLAN
  and restart recovery.
- Retains direct OVSDB code for maintenance and testing only; advanced OVSDB
  acceptance is deferred.
- Does not claim production readiness, arbitrary foreign-state preservation,
  missed-DEL/orphan recovery, or Kubernetes API-outage acceptance.

See [the release notes](docs/release/v0.1.0.md).
