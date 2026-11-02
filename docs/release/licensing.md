# Licensing and attribution

## Project code

The original project code is licensed under the Apache License, Version 2.0;
the complete, unmodified license text is in [`../../LICENSE`](../../LICENSE).
This licensing statement does not change the license or attribution of
third-party material. No copyright owner or year is asserted here because the
repository does not establish one.

## Generated and adapted source

The 40 files in `services/ovsdb_mgmt/ovnnb/` identify themselves as generated
by `libovsdb.modelgen`. The checked-in regeneration script,
`tools/regen-ovn-nb-models.sh`, identifies OVN v26.09.0 as its default input
schema release. The generated `model.go` embeds an `OVN_Northbound` schema with
version `7.19.0`, and `schema_compat_test.go` checks that version and
representative schema fields.

The generated model provenance was independently reproduced against the
official OVN v26.09.0 schema at
[`ovn-nb.ovsschema`](https://github.com/ovn-org/ovn/blob/v26.09.0/ovn-nb.ovsschema).
The read-only fetch returned 45,507 bytes with SHA-256
`e3dabf87e82201255d12fb719d051acb61d394700451bb73c8fa195b9abbd617`; the
source declares schema name `OVN_Northbound`, version `7.19.0`, and checksum
`2631744256 45474`. The source repository's official
[`LICENSE`](https://github.com/ovn-org/ovn/blob/v26.09.0/LICENSE) identifies
the work as Apache-2.0 and states `Copyright The Open vSwitch Authors`.
The fetched license was 10,767 bytes with SHA-256
`f41f887b04dd604250193ddd88691ecd168dacdecca2d0d6581d8840e3f0b0dc`.
That attribution is retained in [`../../NOTICE`](../../NOTICE); no copyright
year is supplied because the source notice supplies none.

For the reproducibility check, the schema was fetched into a scratch directory
inside the repository and passed to the same `go run -mod=readonly
github.com/ovn-kubernetes/libovsdb/cmd/modelgen -p ovnnb -o <output>
<schema>` generator invocation used by `tools/regen-ovn-nb-models.sh`. The
wrapper itself was not run: it deletes and replaces the checked-in model
directory. All 40 generated outputs byte-matched the checked-in model files;
the scratch directory was then removed. The embedded schema in `model.go` is
the generator's libovsdb representation, not a byte-for-byte copy of the
upstream JSON. The provenance comparison is based on reproducing all generated
files from the official source input. The model generator is from
`github.com/ovn-kubernetes/libovsdb`; its pinned dependency license and
attribution are also recorded in [`../../NOTICE`](../../NOTICE).

The six files under `gen/` are marked as generated from the repository's own
three `.proto` files. Their headers report `protoc-gen-go v1.36.11`,
`protoc-gen-go-grpc` versions v1.4.0, v1.5.1, and v1.6.2 across the generated
files, and `protoc v3.21.12`; the `.proto` inputs are present under `proto/`.
No copied third-party `.proto` source was identified in those inputs.

## Verified Go dependency findings

The module graph inventory below was made from `go.mod`, `go.sum`,
`go list -m all`, and license/notice files available in the local Go module
cache. The module graph contains entries that do not provide packages to this
project's source build or tests; its totals are not the count of dependencies
whose code is bundled in a source-only publication:

| Inventory | Count |
| --- | ---: |
| Go module requirements declared in `go.mod` | 91 |
| Direct requirements | 17 |
| Indirect requirements | 74 |
| External modules in `go list -m all` (excluding this project) | 270 |
| `go.sum` checksum records | 250 |
| Distinct module/version pairs represented in `go.sum` | 130 |
| Resolved modules with a root license document found locally | 158 |
| Resolved modules without locally available root license evidence | 112 |

The 158 locally documented module roots classify by inspected license text as
Apache-2.0 (74), MIT (50), BSD-3-Clause (29), BSD-2-Clause (4), and ISC (1).
The local cache contained extracted source for 120 resolved modules and
archives for 39; one available archive, `github.com/NYTimes/gziphandler`,
contained no root license document. These aggregate counts describe available
evidence, not a distribution of bundled third-party source.

On this Linux/amd64 environment, `go list -deps -test ./...` completed with
exit status 0, no package errors, and 1,030 packages (including standard
library and project packages). The graph contains 90 distinct external
modules contributing packages to the source build or tests. All 90 have a
root license file in their version-specific local module source. The license
texts classify as Apache-2.0 (39), MIT (33), BSD-3-Clause (17), and ISC (1).
Thus none of the 112 modules listed below for missing local-cache evidence
contributes a package to this command's source build or tests. The remaining
180 resolved modules likewise did not appear in that package graph; 68 of
those have root license evidence locally and are not listed below.

These are source-build/test dependency findings, not a claim that the source
distribution bundles the dependency sources. The repository has no vendored
Go dependency tree; dependency contents are not included in this source-only
publication, apart from the generated OVN schema-derived models attributed
above. `NOTICE` preserves the ten third-party NOTICE files found in locally
available module source. A binary or other package bundling dependencies
would need its own distribution-specific review and inclusion of the
applicable license texts and notices.

All 17 direct requirements below have a root `LICENSE` file in the local module
cache. The license names are based on those files, not inferred from project
names or websites.

| Module | Version from `go.mod` | License evidence |
| --- | --- | --- |
| `github.com/cenkalti/backoff/v4` | `v4.3.0` | MIT (`LICENSE`) |
| `github.com/containernetworking/cni` | `v1.3.0` | Apache-2.0 (`LICENSE`) |
| `github.com/go-logr/logr` | `v1.4.3` | Apache-2.0 (`LICENSE`) |
| `github.com/google/uuid` | `v1.6.0` | BSD-3-Clause (`LICENSE`) |
| `github.com/osrg/gobgp/v4` | `v4.5.0` | Apache-2.0 (`LICENSE`) |
| `github.com/ovn-kubernetes/libovsdb` | `v0.8.2-0.20260302130604-c07ce22366ac` | Apache-2.0 (`LICENSE`); attribution in `NOTICE` |
| `github.com/ovn-kubernetes/ovn-kubernetes/go-controller` | `v0.0.0-20260501210012-834f7acbb6e0` | Apache-2.0 (`LICENSE`) |
| `github.com/vishvananda/netlink` | `v1.3.2-0.20260320193013-72a8cd7e0a73` | Apache-2.0 (`LICENSE`) |
| `github.com/vishvananda/netns` | `v0.0.5` | Apache-2.0 (`LICENSE`) |
| `golang.org/x/sys` | `v0.40.0` | BSD-3-Clause (`LICENSE`) |
| `google.golang.org/grpc` | `v1.80.0` | Apache-2.0 (`LICENSE`); attribution in `NOTICE` |
| `google.golang.org/protobuf` | `v1.36.11` | BSD-3-Clause (`LICENSE`) |
| `k8s.io/api` | `v0.35.1` | Apache-2.0 (`LICENSE`) |
| `k8s.io/apimachinery` | `v0.35.1` | Apache-2.0 (`LICENSE`) |
| `k8s.io/client-go` | `v0.35.1` | Apache-2.0 (`LICENSE`) |
| `sigs.k8s.io/controller-runtime` | `v0.23.1` | Apache-2.0 (`LICENSE`) |
| `sigs.k8s.io/yaml` | `v1.6.0` | Apache-2.0 (`LICENSE`) |

## Unused modules without local-cache license evidence

The following 112 modules have no root license evidence in the inspected local
cache. They are all absent from the `go list -deps -test ./...` package graph
above and therefore are not required to provide a package to that source
build/test selection. For 111, the module source archive was not available in
the local cache; the `github.com/NYTimes/gziphandler` archive was available
but contained no root license file. Their licenses were not inferred or
declared here. These entries are not a blocker to the stated source-only
publication because no module contents are being redistributed. If a
different build target or distribution mode brings one of these modules'
contents into a release package, verify its exact-version license and notices
before distributing that package. The project license does not relicense
these modules.

| Module | Version |
| --- | --- |
| `cloud.google.com/go/compute/metadata` | `v0.9.0` |
| `dario.cat/mergo` | `v1.0.0` |
| `github.com/Azure/go-ansiterm` | `v0.0.0-20230124172434-306776ec8161` |
| `github.com/BurntSushi/toml` | `v1.5.0` |
| `github.com/GoogleCloudPlatform/opentelemetry-operations-go/detectors/gcp` | `v1.31.0` |
| `github.com/Microsoft/go-winio` | `v0.6.2` |
| `github.com/Microsoft/hcsshim` | `v0.13.0` |
| `github.com/NYTimes/gziphandler` | `v1.1.1` |
| `github.com/Nvveen/Gotty` | `v0.0.0-20120604004816-cd527374f1e5` |
| `github.com/alecthomas/kingpin/v2` | `v2.4.0` |
| `github.com/alecthomas/units` | `v0.0.0-20211218093645-b94a6e3cc137` |
| `github.com/alexflint/go-filemutex` | `v1.3.0` |
| `github.com/armon/go-socks5` | `v0.0.0-20160902184237-e75332964ef5` |
| `github.com/asaskevich/govalidator` | `v0.0.0-20210307081110-f21760c49a8d` |
| `github.com/bhendo/go-powershell` | `v0.0.0-20190719160123-219e7fb4e41e` |
| `github.com/cncf/xds/go` | `v0.0.0-20251210132809-ee656c7534f5` |
| `github.com/containerd/cgroups` | `v1.1.0` |
| `github.com/containerd/continuity` | `v0.4.5` |
| `github.com/coreos/go-iptables` | `v0.8.0` |
| `github.com/cpuguy83/go-md2man/v2` | `v2.0.6` |
| `github.com/docker/cli` | `v27.4.1+incompatible` |
| `github.com/docker/docker` | `v28.0.0+incompatible` |
| `github.com/docker/go-connections` | `v0.5.0` |
| `github.com/docker/go-units` | `v0.5.0` |
| `github.com/dustin/go-humanize` | `v1.0.1` |
| `github.com/envoyproxy/go-control-plane` | `v0.14.0` |
| `github.com/envoyproxy/go-control-plane/envoy` | `v1.36.0` |
| `github.com/envoyproxy/go-control-plane/ratelimit` | `v0.1.0` |
| `github.com/envoyproxy/protoc-gen-validate` | `v1.3.0` |
| `github.com/gaissmai/cidrtree` | `v0.1.4` |
| `github.com/go-jose/go-jose/v4` | `v4.1.4` |
| `github.com/godbus/dbus/v5` | `v5.1.0` |
| `github.com/golang-jwt/jwt/v5` | `v5.2.2` |
| `github.com/golang/glog` | `v1.2.5` |
| `github.com/google/gopacket` | `v1.1.19` |
| `github.com/google/shlex` | `v0.0.0-20191202100458-e7afc7fbc510` |
| `github.com/gorilla/mux` | `v1.8.0` |
| `github.com/gregjones/httpcache` | `v0.0.0-20190611155906-901d90724c79` |
| `github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus` | `v1.0.1` |
| `github.com/grpc-ecosystem/go-grpc-middleware/v2` | `v2.3.0` |
| `github.com/jonboulle/clockwork` | `v0.5.0` |
| `github.com/josharian/native` | `v1.1.0` |
| `github.com/jpillora/backoff` | `v1.0.0` |
| `github.com/juju/errors` | `v0.0.0-20200330140219-3fe23663418f` |
| `github.com/juju/testing` | `v0.0.0-20200706033705-4c23f9c453cd` |
| `github.com/julienschmidt/httprouter` | `v1.3.0` |
| `github.com/k8snetworkplumbingwg/govdpa` | `v0.1.5-0.20230926073613-07c1031aea47` |
| `github.com/k8snetworkplumbingwg/ipamclaims` | `v0.5.1-alpha` |
| `github.com/k8snetworkplumbingwg/multi-networkpolicy` | `v1.0.1` |
| `github.com/k8snetworkplumbingwg/network-attachment-definition-client` | `v1.7.7` |
| `github.com/k8snetworkplumbingwg/sriovnet` | `v1.2.1-0.20250818105516-24ab680f94f3` |
| `github.com/mdlayher/arp` | `v0.0.0-20220512170110-6706a2966875` |
| `github.com/mdlayher/ethernet` | `v0.0.0-20220221185849-529eae5b6118` |
| `github.com/mdlayher/ndp` | `v1.0.1` |
| `github.com/mdlayher/packet` | `v1.1.2` |
| `github.com/mdlayher/socket` | `v0.5.1` |
| `github.com/metallb/frr-k8s` | `v0.0.21` |
| `github.com/miekg/dns` | `v1.1.31` |
| `github.com/mitchellh/copystructure` | `v1.2.0` |
| `github.com/mitchellh/reflectwalk` | `v1.0.2` |
| `github.com/moby/docker-image-spec` | `v1.3.1` |
| `github.com/moby/sys/user` | `v0.3.0` |
| `github.com/moby/sys/userns` | `v0.1.0` |
| `github.com/moby/term` | `v0.5.0` |
| `github.com/mwitkow/go-conntrack` | `v0.0.0-20190716064945-2f068394615f` |
| `github.com/opencontainers/go-digest` | `v1.0.0` |
| `github.com/opencontainers/image-spec` | `v1.1.0` |
| `github.com/opencontainers/runc` | `v1.2.8` |
| `github.com/openshift/api` | `v0.0.0-20260311143357-f6ee4c095675` |
| `github.com/openshift/client-go` | `v0.0.0-20260306160707-3935d929fc7d` |
| `github.com/openshift/custom-resource-status` | `v1.1.2` |
| `github.com/ory/dockertest/v3` | `v3.12.0` |
| `github.com/pborman/uuid` | `v1.2.0` |
| `github.com/peterbourgon/diskv` | `v2.0.1+incompatible` |
| `github.com/planetscale/vtprotobuf` | `v0.6.1-0.20240319094008-0393e58bdf10` |
| `github.com/russross/blackfriday/v2` | `v2.1.0` |
| `github.com/safchain/ethtool` | `v0.6.2` |
| `github.com/sirupsen/logrus` | `v1.9.3` |
| `github.com/soheilhy/cmux` | `v0.1.5` |
| `github.com/spiffe/go-spiffe/v2` | `v2.6.0` |
| `github.com/tmc/grpc-websocket-proxy` | `v0.0.0-20220101234140-673ab2c3ae75` |
| `github.com/urfave/cli/v2` | `v2.27.2` |
| `github.com/xeipuuv/gojsonpointer` | `v0.0.0-20190905194746-02993c407bfb` |
| `github.com/xeipuuv/gojsonreference` | `v0.0.0-20180127040603-bd5ef7bd5415` |
| `github.com/xeipuuv/gojsonschema` | `v1.2.0` |
| `github.com/xhit/go-str2duration/v2` | `v2.1.0` |
| `github.com/xiang90/probing` | `v0.0.0-20221125231312-a49e3df8f510` |
| `github.com/xrash/smetrics` | `v0.0.0-20240312152122-5f08fbb34913` |
| `go.etcd.io/bbolt` | `v1.4.3` |
| `go.etcd.io/etcd/pkg/v3` | `v3.6.5` |
| `go.etcd.io/etcd/server/v3` | `v3.6.5` |
| `go.etcd.io/raft/v3` | `v3.6.0` |
| `go.opencensus.io` | `v0.24.0` |
| `go.opentelemetry.io/contrib/detectors/gcp` | `v1.39.0` |
| `golang.org/x/tools/go/expect` | `v0.1.0-deprecated` |
| `golang.org/x/tools/go/packages/packagestest` | `v0.1.1-deprecated` |
| `google.golang.org/grpc/security/advancedtls` | `v0.0.0-20240425232638-1e8b9b7fc655` |
| `gopkg.in/fsnotify/fsnotify.v1` | `v1.4.7` |
| `gopkg.in/gcfg.v1` | `v1.2.3` |
| `gopkg.in/k8snetworkplumbingwg/multus-cni.v4` | `v4.2.5-0.20260316195000-6eb8514eef3c` |
| `gopkg.in/warnings.v0` | `v0.1.2` |
| `k8s.io/code-generator` | `v0.35.1` |
| `k8s.io/component-helpers` | `v0.35.1` |
| `k8s.io/controller-manager` | `v0.35.1` |
| `k8s.io/gengo/v2` | `v2.0.0-20250922181213-ec3ebc5fd46b` |
| `k8s.io/kubelet` | `v0.35.1` |
| `k8s.io/kubernetes` | `v1.35.1` |
| `kubevirt.io/api` | `v1.0.0-alpha.0` |
| `kubevirt.io/containerized-data-importer-api` | `v1.55.0` |
| `kubevirt.io/controller-lifecycle-operator-sdk/api` | `v0.0.0-20220329064328-f3cc58c6ed90` |
| `sigs.k8s.io/knftables` | `v0.0.20` |
| `sigs.k8s.io/network-policy-api` | `v0.1.5` |
