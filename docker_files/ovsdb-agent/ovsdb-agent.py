from ovsdbapp.backend.ovs_idl import connection
from ovsdbapp.schema.open_vswitch import impl_idl as schema_ovs
from ovsdbapp.schema.ovn_northbound import impl_idl as schema_ovnnb

import sys
sys.path.append('/config')
from app_settings import OVS_NAME, OVSDB_OVS_CONN, OVSDB_OVNNB_CONN, RABBITMQ_SERVER, RABBIT_USER, RABBIT_PASSWORD, VXLAN_PORT

from oslo_config import cfg
import oslo_messaging as om
import time
import logging
import eventlet
eventlet.monkey_patch()

logger = logging.getLogger(__name__)

OVS_DB, OVNNB_DB = 'Open_vSwitch', 'OVN_Northbound'
OVSDB_SCHEMA = {OVS_DB: {'Idl': schema_ovs.OvsdbIdl, 'conn': None},
                OVNNB_DB: {'Idl': schema_ovnnb.OvnNbApiIdlImpl, 'conn': None}}

# Invoke "get_transport". This call will set default Configurations required to Create Messaging Transport
transport = om.get_transport(cfg.CONF)

cfg.CONF.set_override(
    'transport_url', 'rabbit://{}:{}@{}:5672//'.format(RABBIT_USER, RABBIT_PASSWORD, RABBITMQ_SERVER))

# Create Messaging Transport
transport = om.get_transport(cfg.CONF)

# Request context dict
ctxt = {}


# Create Target (Exchange, Topic and Server to listen on)
target_ovn = om.Target(topic='ovn_bus', server=RABBITMQ_SERVER)


def to_int(i, base):
    return int(str(i), base)


class OVSDB():
    def __init__(self):
        # ToDo: init for rabbitmq connection
        self.logger = logger

    def _ovsdb_conn(self, conn, schema):
        if schema in OVSDB_SCHEMA:
            if OVSDB_SCHEMA[schema]['conn']:
                return OVSDB_SCHEMA[schema]['conn']
            # The python-ovs Idl class (Open vSwitch Database Interface Definition Language).
            # Take it from server's database
            try:
                # The ovsdbapp Connection object
                i = connection.OvsdbIdl.from_server(conn, schema)
            except Exception as e:
                self.logger.exception('Could not retrieve schema {} from {}.\n{}'.format(schema, conn, e))
                return None

            try:
                # The ovsdbapp Connection object
                conn = connection.Connection(idl=i, timeout=3)
            except Exception as e:
                self.logger.exception('Cannot initiate OVSDB connection: {}', format(e))
                return None

            # The OVN_Northbound API implementation object
            api = OVSDB_SCHEMA[schema]['Idl'](conn)
            OVSDB_SCHEMA[schema]['conn'] = api
            return api
        else:
            self.logger.exception('Incorrect OVSDB schema: {}', format(schema))
            return None

    def _ovsdb_exec(self, conn, schema, cmd, *args):
        api = self._ovsdb_conn(conn, schema)
        if api:
            try:
                res = getattr(api, cmd)(*args).execute()
            except Exception as e:
                self.logger.exception('Cannot execute OVSDB command: %s', e)
                return None
            if res == None:
                res = True
            return res
        return None

    def update_lsp_addr(self, ctx, port, address, action):
        # Get current addresses of lsp
        args = ['Logical_Switch_Port', port, 'addresses']
        addresses = self._ovsdb_exec(OVSDB_OVNNB_CONN, OVNNB_DB, 'db_get', *args)
        if addresses == None:
            return None

        # Update addresses
        if type(addresses) != list:
            addresses = [addresses]
        addresses = set(list(addresses))
        getattr(addresses, action)(address)
        args = ['Logical_Switch_Port', port, ('addresses', list(addresses))]
        res = self._ovsdb_exec(OVSDB_OVNNB_CONN, OVNNB_DB, 'db_set', *args)
        if not res:
            return None

        return True

    def get_vxlan_port(self, ctx, remote_ip, key):
        # Search VXLAN port named 'vxlan_<remote_ip>_<key>'
        args = [OVS_NAME]
        ports_ovs = self._ovsdb_exec(OVSDB_OVS_CONN, OVS_DB, 'list_ports', *args)
        if not ports_ovs:
            return None
        vxlan_port_name = 'vxlan_{}_{}'.format(remote_ip, key)
        if vxlan_port_name in ports_ovs:
            return vxlan_port_name
        else:
            return None

    # def add_vxlan_port(self, ctx, remote_ip, key, logical_switch):
    def add_vxlan_port(self, ctx, remote_ip, key, logical_switch):
        vxlan_port_name = 'vxlan_{}_{}'.format(remote_ip, key)

        ctx = {}
        vxlan_port = self.get_vxlan_port(ctx, remote_ip, key)
        if vxlan_port is not None:
            return vxlan_port

        # Add VXLAN port named 'vxlan_<remote_ip>_<key>' to OVS bridge
        args = [OVS_NAME, vxlan_port_name]
        res = self._ovsdb_exec(OVSDB_OVS_CONN, OVS_DB, 'add_port', *args)
        if not res:
            return None

        # waiting for vxlan port creation
        time.sleep(2)

        args = ['Interface', vxlan_port_name, ('type', 'vxlan'),
                ('options', {'key': str(key), 'remote_ip': remote_ip, 'dst_port': VXLAN_PORT}),
                ('external_ids', {'iface-id': vxlan_port_name})]
        _ = self._ovsdb_exec(OVSDB_OVS_CONN, OVS_DB, 'db_set', *args)

        # Create logical port named 'vxlan_<remote_ip>_<key>' as physical port
        args = [logical_switch, vxlan_port_name]
        res = self._ovsdb_exec(OVSDB_OVNNB_CONN, OVNNB_DB, 'lsp_add', *args)
        if not res:
            return None

        return vxlan_port_name

    def del_vxlan_port(self, ctx, remote_ip, key):
        vxlan_port_name = 'vxlan_{}_{}'.format(remote_ip, key)
        args = [vxlan_port_name]

        #Delete vxlan port from OVSDB OVS
        res = self._ovsdb_exec(OVSDB_OVS_CONN, OVS_DB, 'del_port', *args)
        if not res:
            return None

        # Delete vxlan port from OVSDB OVNNB
        res = self._ovsdb_exec(OVSDB_OVNNB_CONN, OVNNB_DB, 'lsp_del', *args)
        if not res:
            return None

        return vxlan_port_name

    # Event handlers for BGP


##Create EndPoint List
endpoints = [OVSDB(), ]

##Create RPC Server
server = om.get_rpc_server(transport, target_ovn, endpoints, executor='threading')

##Start RPC Server
try:
    server.start()
    while True:
        time.sleep(1)
except KeyboardInterrupt:
    print("Stopping server")

server.stop()
server.wait()
