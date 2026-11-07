FROM evpn4ovn/node-agent-test:cni-24b-base
COPY operator /usr/local/bin/evpn-core-operator
COPY evpn-node-agent /usr/local/bin/evpn-node-agent
COPY provider-grpc-proxy /usr/local/bin/provider-grpc-proxy
COPY core-resources.yaml /core-resources.yaml
ENTRYPOINT ["/usr/local/bin/evpn-core-operator"]
