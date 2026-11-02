# Quick Start

This default workflow only builds and tests the Go code. It does not create a
cluster, configure host networking, or modify OVN/OVS state.
The project is experimental; read [deployment security limits](SECURITY.md)
before using any live example. Downloaded Go modules are third-party source;
"offline" means no infrastructure provisioning, not a guarantee of no downloads.

## Prerequisites

- Go 1.25.0, as declared in [`go.mod`](go.mod)
- Network access on first use to download Go modules, unless they are already
  cached

Run these commands from the repository root:

```bash
go version
go test ./...
go build ./cmd/evpn-cni ./cmd/evpn-node-agent ./cmd/evpn-ovsdb \
  ./services/evpn_mgmt ./services/ovsdb_mgmt ./services/bgp_ad_handler \
  ./deploy/operator
```

By default, tests that require a disposable Kubernetes, OVN/OVS, GoBGP/FRR, or
privileged Linux networking environment are skipped. Do not set live-test
environment flags unless following the corresponding isolated test procedure
in the [Testing Guide](TESTING_GUIDE.md).

For the project overview, status, and architecture links, see the
[README](README.md). Optional lab workflows are documented separately in the
[Testing Guide](TESTING_GUIDE.md); they are not part of this safe default.

The live suites use digest-pinned FRR 10.7.1 and managed k8gobgp v0.2.8.
Their [runtime dependencies and ownership](docs/architecture/runtime-dependencies.md)
are separate from the Go module prerequisites above.
**Containerlab is the standard and required environment provisioning mechanism
for all EVPN-for-OVN E2E tests.** Each runner invokes the
[common laboratory lifecycle](deploy/containerlab/README.md) automatically;
do not pre-create a cluster. Unit tests, builds and ordinary Framework runtime
deployment do not require Containerlab.
