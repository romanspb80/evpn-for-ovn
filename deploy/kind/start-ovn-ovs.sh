#!/bin/sh
set -eu

/usr/share/openvswitch/scripts/ovs-ctl --system-id=random start

# Pod network namespace survives container restarts, so a stale br-int kernel
# netdev can remain while OVSDB is re-created from scratch. Remove it before
# re-adding the bridge to avoid "could not add network device br-int ... File exists".
if ! ovs-vsctl br-exists br-int; then
  if ip link show br-int >/dev/null 2>&1; then
    ip link del br-int || true
  fi
  ovs-vsctl --may-exist add-br br-int
fi
ip link set br-int up || true

/usr/share/ovn/scripts/ovn-ctl --db-nb-create-insecure-remote=yes --db-sb-create-insecure-remote=yes start_northd
ovs-appctl -t ovsdb-server ovsdb-server/add-remote ptcp:6640:0.0.0.0 || true
ovn-nbctl set-connection ptcp:6641:0.0.0.0 || true

LOCAL_IP="${OVN_EVPN_LOCAL_IP:-}"
if [ -z "${LOCAL_IP}" ]; then
  LOCAL_IP="$(ip -4 -o addr show scope global | awk '{split($4,a,"/"); print a[1]; exit}')"
fi

EVPN_VXLAN_PORTS="${OVN_EVPN_VXLAN_PORTS:-}"

ovs-vsctl set Open_vSwitch . external-ids:ovn-remote='tcp:127.0.0.1:6642'
ovs-vsctl set Open_vSwitch . external-ids:ovn-encap-type='vxlan'
ovs-vsctl set Open_vSwitch . external-ids:ovn-encap-ip="${LOCAL_IP}"
if [ -n "${EVPN_VXLAN_PORTS}" ]; then
  ovs-vsctl set Open_vSwitch . external-ids:ovn-evpn-local-ip="${LOCAL_IP}"
  ovs-vsctl set Open_vSwitch . external-ids:ovn-evpn-vxlan-ports="${EVPN_VXLAN_PORTS}"
else
  ovs-vsctl remove Open_vSwitch . external-ids ovn-evpn-local-ip || true
  ovs-vsctl remove Open_vSwitch . external-ids ovn-evpn-vxlan-ports || true
fi

/usr/share/ovn/scripts/ovn-ctl start_controller

tail -f /var/log/openvswitch/ovs-vswitchd.log /var/log/ovn/ovn-northd.log /var/log/ovn/ovn-controller.log
