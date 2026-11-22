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

The default generator input is the official OVN v26.09.0 schema at
[`ovn-nb.ovsschema`](https://github.com/ovn-org/ovn/blob/v26.09.0/ovn-nb.ovsschema).
The source repository's official
[`LICENSE`](https://github.com/ovn-org/ovn/blob/v26.09.0/LICENSE) identifies
the work as Apache-2.0 and states `Copyright The Open vSwitch Authors`.
That attribution is retained in [`../../NOTICE`](../../NOTICE); no copyright
year is supplied because the source notice supplies none.

To check reproducibility without overwriting the models, fetch the official
schema into a private scratch directory and use `go run -mod=readonly
github.com/ovn-kubernetes/libovsdb/cmd/modelgen -p ovnnb -o <output>
<schema>`, matching `tools/regen-ovn-nb-models.sh`. Compare the generated
files with the checked-in models, then remove only that scratch directory.
Do not use the wrapper for a read-only comparison: it deletes and replaces
the checked-in model directory. The embedded schema in `model.go` is
the generator's libovsdb representation, not a byte-for-byte copy of the
upstream JSON. Compare generated files rather than embedded JSON formatting.
The model generator is from
`github.com/ovn-kubernetes/libovsdb`; its pinned dependency license and
attribution are also recorded in [`../../NOTICE`](../../NOTICE).

The six files under `gen/` are marked as generated from the repository's own
three `.proto` files. Their headers report `protoc-gen-go v1.36.11`,
`protoc-gen-go-grpc` versions v1.4.0, v1.5.1, and v1.6.2 across the generated
files, and `protoc v3.21.12`; the `.proto` inputs are present under `proto/`.
No copied third-party `.proto` source was identified in those inputs.

## Go dependency licensing

Use `go.mod`, `go.sum`, `go list -m all`, and exact-version license/notice
files to review the selected module graph. Use `go list -deps -test ./...`
to identify packages for that build/test selection; repeat the review for
different targets or distribution modes. A resolved module is not necessarily
a bundled dependency, and a missing local cache file is not a license finding.

The repository has no vendored
Go dependency tree; dependency contents are not included in this source-only
publication, apart from the generated OVN schema-derived models attributed
above. `NOTICE` preserves the ten third-party NOTICE files found in locally
available module source. A binary or other package bundling dependencies
would need its own distribution-specific review and inclusion of the
applicable license texts and notices.

The direct-version license mapping below records the applicable root
`LICENSE` texts, not licenses inferred from project names or websites.
Check the exact inputs and distribution requirements when packaging binaries.

| Module | Version from `go.mod` | License evidence |
| --- | --- | --- |
| `github.com/cenkalti/backoff/v4` | `v4.3.0` | MIT (`LICENSE`) |
| `github.com/containernetworking/cni` | `v1.3.0` | Apache-2.0 (`LICENSE`) |
| `github.com/go-logr/logr` | `v1.4.3` | Apache-2.0 (`LICENSE`) |
| `github.com/google/uuid` | `v1.6.0` | BSD-3-Clause (`LICENSE`) |
| `github.com/osrg/gobgp/v4` | `v4.7.0` | Apache-2.0 (`LICENSE`) |
| `github.com/ovn-kubernetes/libovsdb` | `v0.8.2-0.20260302130604-c07ce22366ac` | Apache-2.0 (`LICENSE`); attribution in `NOTICE` |
| `github.com/ovn-kubernetes/ovn-kubernetes/go-controller` | `v0.0.0-20260501210012-834f7acbb6e0` | Apache-2.0 (`LICENSE`) |
| `github.com/vishvananda/netlink` | `v1.3.2-0.20260320193013-72a8cd7e0a73` | Apache-2.0 (`LICENSE`) |
| `github.com/vishvananda/netns` | `v0.0.5` | Apache-2.0 (`LICENSE`) |
| `golang.org/x/sys` | `v0.47.0` | BSD-3-Clause (`LICENSE`) |
| `google.golang.org/grpc` | `v1.83.2` | Apache-2.0 (`LICENSE`); attribution in `NOTICE` |
| `google.golang.org/protobuf` | `v1.36.11` | BSD-3-Clause (`LICENSE`) |
| `k8s.io/api` | `v0.35.1` | Apache-2.0 (`LICENSE`) |
| `k8s.io/apiextensions-apiserver` | `v0.35.1` | Apache-2.0 (`LICENSE`) |
| `k8s.io/apimachinery` | `v0.35.1` | Apache-2.0 (`LICENSE`) |
| `k8s.io/client-go` | `v0.35.1` | Apache-2.0 (`LICENSE`) |
| `k8s.io/cri-api` | `v0.35.1` | Apache-2.0 (`LICENSE`) |
| `sigs.k8s.io/controller-runtime` | `v0.23.1` | Apache-2.0 (`LICENSE`) |
| `sigs.k8s.io/yaml` | `v1.6.0` | Apache-2.0 (`LICENSE`) |

## Additional module license-review inventory

The following module/version inventory retains the previously identified
license-review candidates. It is not a current package-usage assessment or
an assertion that these modules lack licenses. No license is inferred or
declared for these entries. If the selected build target or distribution
bundles any of their contents, inspect the exact-version license and notices
before distribution. The project license does not relicense these modules.

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
