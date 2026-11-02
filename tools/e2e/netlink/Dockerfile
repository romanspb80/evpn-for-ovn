ARG GO_BUILD_IMAGE
ARG NETLINK_RUNTIME_IMAGE
FROM ${GO_BUILD_IMAGE} AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg ./pkg
COPY gen/evpn_mgmt ./gen/evpn_mgmt
COPY gen/ovsdb_mgmt ./gen/ovsdb_mgmt
COPY services/ovsdb_mgmt/ovnnb ./services/ovsdb_mgmt/ovnnb
COPY services/bgp_ad_handler ./services/bgp_ad_handler
COPY tools/e2e/netlink/route ./tools/e2e/netlink/route
RUN CGO_ENABLED=0 go build -o /out/bgp-ad-handler ./services/bgp_ad_handler
RUN CGO_ENABLED=0 go build -o /out/evpn-netlink-route ./tools/e2e/netlink/route

FROM ${NETLINK_RUNTIME_IMAGE}
COPY --from=builder /out/bgp-ad-handler /usr/local/bin/bgp-ad-handler
COPY --from=builder /out/evpn-netlink-route /usr/local/bin/evpn-netlink-route
ENTRYPOINT ["/usr/local/bin/bgp-ad-handler"]
