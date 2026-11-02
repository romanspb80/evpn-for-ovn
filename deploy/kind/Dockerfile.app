FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/ovsdb_mgmt ./services/ovsdb_mgmt && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/evpn_mgmt ./services/evpn_mgmt && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/bgp_ad_handler ./services/bgp_ad_handler

FROM debian:sid-slim AS ovn-tools
# Get ovn-nbctl from OVN package
RUN apt-get update && apt-get install -y --no-install-recommends \
    ovn-common \
    && rm -rf /var/lib/apt/lists/*

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates iproute2 iputils-ping tcpdump netcat-openbsd \
    libunbound8 \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /out/ovsdb_mgmt /usr/local/bin/ovsdb_mgmt
COPY --from=builder /out/evpn_mgmt /usr/local/bin/evpn_mgmt
COPY --from=builder /out/bgp_ad_handler /usr/local/bin/bgp_ad_handler
# Copy ovn-nbctl from OVN container
COPY --from=ovn-tools /usr/bin/ovn-nbctl /usr/local/bin/ovn-nbctl
