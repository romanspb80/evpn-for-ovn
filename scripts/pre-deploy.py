#!/usr/bin/env python3

import subprocess
from ovsdbapp.backend.ovs_idl import connection
from ovsdbapp.schema.open_vswitch import impl_idl as schema_ovs
from ovsdbapp.schema.ovn_northbound import impl_idl as schema_ovnnb
from settings import OVS_BRIDGE, OVSDB_OVS_CONN, OVSDB_OVNNB_CONN, OVS_PORT, OVN_SWITCH, LSP, VM1_IP, VM1_MAC, NETNS1
import logging

try:
    subprocess.run(['sudo', 'ovn-nbctl', 'set-connection', 'ptcp:6641:192.168.10.20'], check=True)
except subprocess.CalledProcessError:
    pass

try:
    subprocess.run(['sudo', 'ovs-vsctl', 'set-manager', 'ptcp:6640'], check=True)
except subprocess.CalledProcessError:
    pass

OVS_DB, OVNNB_DB = 'Open_vSwitch', 'OVN_Northbound'

logger = logging.getLogger(__name__)


# Setup OVN
def cmd_ovsdb(api, cmd, *args):
    res = getattr(api, cmd)(*args).execute()
    return res


i = connection.OvsdbIdl.from_server(OVSDB_OVNNB_CONN, OVNNB_DB)
c = connection.Connection(idl=i, timeout=3)
api = schema_ovnnb.OvnNbApiIdlImpl(c)

args = [OVN_SWITCH]
print('Creating Logical Switch {}'.format(*args))
_ = cmd_ovsdb(api, 'ls_add', *args)

args = [OVN_SWITCH, LSP]
print('Creating port {1} in Logical Switch {0}'.format(*args))
_ = cmd_ovsdb(api, 'lsp_add', *args)


args = [LSP, [VM1_MAC]]
print('Setup port {0} with mac address {1}'.format(*args))
_ = cmd_ovsdb(api, 'lsp_set_addresses', *args)


# Setup OVS
i = connection.OvsdbIdl.from_server(OVSDB_OVS_CONN, OVS_DB)
c = connection.Connection(idl=i, timeout=3)
api = schema_ovs.OvsdbIdl(c)

args = [OVS_BRIDGE, OVS_PORT]
print('Creating port {1} in OVS Switch {0}'.format(*args))
_ = cmd_ovsdb(api, 'add_port', *args)


args = ['Interface', OVS_PORT, ('type', 'internal'), ('external_ids', {'iface-id': LSP})]
print('Mapping OVS Interface {} with Logical Switch Port {}'.format(OVS_PORT, LSP))
_ = cmd_ovsdb(api, 'db_set', *args)

# Setup NetNS
cmd_lst = [
    'sudo ip netns add {}'.format(NETNS1),
    'sudo ip link set {} netns {}'.format(OVS_PORT, NETNS1),
    'sudo ip netns exec {} ip link set {} address {}'.format(NETNS1, OVS_PORT, VM1_MAC),
    'sudo ip netns exec {} ip addr add {} dev {}'.format(NETNS1, VM1_IP, OVS_PORT),
    'sudo ip netns exec {} ip link set {} up'.format(NETNS1, OVS_PORT)
]


def cmd_run(cmd):
    try:
        subprocess.run(cmd.split(), check=True)
        res = True
    except subprocess.CalledProcessError:
        res = False
    print(cmd, res)
    return cmd, res


_ = list(map(cmd_run, cmd_lst))
