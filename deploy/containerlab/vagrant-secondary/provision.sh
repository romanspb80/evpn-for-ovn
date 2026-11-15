#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
gateway="${1:-192.168.249.1}"
[[ "$gateway" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]
mkdir -p /etc/systemd/resolved.conf.d
printf '[Resolve]\nDNS=%s\nDNSSEC=no\n' "$gateway" > /etc/systemd/resolved.conf.d/90-evpn-cni-lab.conf
sed -i "s/addresses: \[4.2.2.1, 4.2.2.2, 208.67.220.220\]/addresses: [$gateway]/" /etc/netplan/01-netcfg.yaml
grep -Fq "addresses: [$gateway]" /etc/netplan/01-netcfg.yaml
systemctl restart systemd-resolved
resolvectl dns eth0 "$gateway"
apt-get update -o APT::Update::Error-Mode=any
apt-get install -y ca-certificates curl docker.io git iproute2 openssl python3.11 tcpdump
ln -sfn /usr/bin/python3.11 /usr/local/bin/python3
systemctl enable --now docker
usermod -aG docker vagrant
if [ ! -e /etc/docker/daemon.json ]; then
    test -z "$(docker ps -aq)"
    printf '{"features":{"containerd-snapshotter":false}}\n' > /etc/docker/daemon.json
    systemctl restart docker
else
    python3 -c 'import json; assert json.load(open("/etc/docker/daemon.json")).get("features", {}).get("containerd-snapshotter") is False'
fi

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

fetch() {
    local url="$1" digest="$2" destination="$3"
    curl --fail --location --retry 3 --output "$destination" "$url"
    printf '%s  %s\n' "$digest" "$destination" | sha256sum --check
}

fetch "https://github.com/kubernetes-sigs/kind/releases/download/v0.27.0/kind-linux-amd64" \
    "a6875aaea358acf0ac07786b1a6755d08fd640f4c79b7a2e46681cc13f49a04b" "$work/kind"
fetch "https://dl.k8s.io/release/v1.32.3/bin/linux/amd64/kubectl" \
    "ab209d0c5134b61486a0486585604a616a5bb2fc07df46d304b3c95817b2d79f" "$work/kubectl"
fetch "https://go.dev/dl/go1.27.3.linux-amd64.tar.gz" \
    "65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40" "$work/go.tar.gz"
fetch "https://github.com/srl-labs/containerlab/releases/download/v0.79.0/containerlab_0.79.0_linux_amd64.deb" \
    "a399d92a622b4664d8d1231bc9b7f53a1d210255a0306fa091c3f63779f65f13" "$work/containerlab.deb"
install -m 0755 "$work/kind" "$work/kubectl" /usr/local/bin/
if [ -e /usr/local/go ]; then
    test "$(/usr/local/go/bin/go version)" = "go version go1.27.3 linux/amd64"
else
    tar -C /usr/local -xzf "$work/go.tar.gz"
fi
ln -sfn /usr/local/go/bin/go /usr/local/bin/go
apt-get install -y "$work/containerlab.deb"
usermod -aG clab_admins vagrant
