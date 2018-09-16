import json

from ryu.app.wsgi import Response
from ryu.base import app_manager
from ryu.exception import RyuException
from ryu.lib.ovs import bridge as ovs_bridge
from ryu.lib.packet.bgp import _RouteDistinguisher
from ryu.lib.packet.bgp import EvpnNLRI
from ryu.lib.stringify import StringifyMixin
from ryu.services.protocols.bgp.bgpspeaker import BGPSpeaker
from ryu.services.protocols.bgp.bgpspeaker import RF_L2_EVPN
from ryu.services.protocols.bgp.bgpspeaker import EVPN_MAC_IP_ADV_ROUTE
from ryu.services.protocols.bgp.bgpspeaker import EVPN_MULTICAST_ETAG_ROUTE
from ryu.services.protocols.bgp.info_base.evpn import EvpnPath

import paramiko
import socket

from oslo_config import cfg
import oslo_messaging as om
import time
import eventlet
eventlet.monkey_patch()


# Settings for SSH connection to host with Neutron (DEVSTACK)
USER = 'root'
PASSWORD = 'password'
PORT_SSH = 22

# Network Node IP address (OVS bridge)
DATAPATH_ADDR = '192.168.123.231'
# Neutron Server (OVN Central) IP address
OVNCENTR_ADDR = '192.168.123.231'

# Settings for RabbitMQ Server connection (DEVSTACK)
RABBITMQ_SERVER = '192.168.123.231'
RABBIT_USER = 'stackrabbit'
RABBIT_PASSWORD='password'

API_NAME = 'restvtep'

OVSDB_PORT = 6640  # The IANA registered port for OVSDB [RFC7047]

PRIORITY_D_PLANE = 1
PRIORITY_ARP_REPLAY = 2

TABLE_ID_INGRESS = 0
TABLE_ID_EGRESS = 1

# Invoke "get_transport". This call will set default Configurations required to Create Messaging Transport
transport = om.get_transport(cfg.CONF)

# Set/Override Configurations required to Create Messaging Transport
cfg.CONF.set_override(
    'transport_url', 'rabbit://{}:{}@{}:5672//'.format(RABBIT_USER, RABBIT_PASSWORD, RABBITMQ_SERVER))

# Create Messaging Transport
transport = om.get_transport(cfg.CONF)

# Create Target (Exchange, Topic and Server to listen on)
target = om.Target(topic='ovn_bus', server=RABBITMQ_SERVER)

# Utility functions

def ssh_command(hostname, username, password, port, cmd):
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    out, err = (None, None)
    try:
        client.connect(hostname=hostname, username=username, password=password, port=port)
        stdin, stdout, stderr = client.exec_command(cmd)
        out = stdout.read().decode('ascii').strip('"\n')
        err = stderr.read().decode('ascii').strip('"\n')
    except (paramiko.BadHostKeyException, paramiko.AuthenticationException, paramiko.SSHException, socket.error) as e:
        err = e
    client.close()
    return {'out': out, 'err': err}

def ssh_command_json(hostname, username, password, port, cmd):
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    out, err = (None, None)
    try:
        client.connect(hostname=hostname, username=username, password=password, port=port)
        stdin, stdout, stderr = client.exec_command(cmd)
        out = json.loads(stdout.read())
        err = stderr.read().decode('ascii').strip('"\n')
    except (paramiko.BadHostKeyException, paramiko.AuthenticationException, paramiko.SSHException, socket.error) as e:
        err = e
    client.close()
    return {'out': out, 'err': err}

def to_int(i, base):
    return int(str(i), base)

# Exception classes related to OpenFlow and OVSDB

class RestApiException(RyuException):

    def to_response(self, status):
        body = {
            "error": str(self),
            "status": status,
        }
        return Response(content_type='application/json',
                        body=json.dumps(body), status=status)


# Utility classes related to EVPN

class EvpnSpeaker(BGPSpeaker, StringifyMixin):
    _TYPE = {
        'ascii': [
            'router_id',
        ],
    }

    def __init__(self, dpid, as_number, router_id,
                 best_path_change_handler,
                 peer_down_handler, peer_up_handler,
                 neighbors=None):
        super(EvpnSpeaker, self).__init__(
            as_number=as_number,
            router_id=router_id,
            best_path_change_handler=best_path_change_handler,
            peer_down_handler=peer_down_handler,
            peer_up_handler=peer_up_handler,
            ssh_console=True)

        self.dpid = dpid
        self.as_number = as_number
        self.router_id = router_id
        self.neighbors = neighbors or {}


class EvpnNeighbor(StringifyMixin):
    _TYPE = {
        'ascii': [
            'address',
            'state',
        ],
    }

    def __init__(self, address, remote_as, state='down'):
        super(EvpnNeighbor, self).__init__()
        self.address = address
        self.remote_as = remote_as
        self.state = state


class EvpnNetwork(StringifyMixin):
    _TYPE = {
        'ascii': [
            'route_dist',
            'logical_switch',
        ],
    }

    def __init__(self, vni, logical_switch, route_dist, ethernet_tag_id, clients=None):
        super(EvpnNetwork, self).__init__()
        self.vni = vni
        self.logical_switch = logical_switch
        self.route_dist = route_dist
        self.ethernet_tag_id = ethernet_tag_id
        self.clients = clients or {}

    def get_clients(self, **kwargs):
        l = []
        for _, c in self.clients.items():
            for k, v in kwargs.items():
                if getattr(c, k) != v:
                    break
            else:
                l.append(c)
        return l


class EvpnClient(StringifyMixin):
    _TYPE = {
        'ascii': [
            'mac',
            'ip',
            'next_hop',
            'port',
        ],
    }

    def __init__(self, port, mac, ip, next_hop):
        super(EvpnClient, self).__init__()
        self.port = port
        self.mac = mac
        self.ip = ip
        self.next_hop = next_hop


#class RestVtep(object):
class RestVtep(app_manager.RyuApp):
#     OFP_VERSIONS = [ofproto_v1_3.OFP_VERSION]
#     _CONTEXTS = {'wsgi': WSGIApplication}

    # def __init__(self):
    def __init__(self, *args, **kwargs):
        super(RestVtep, self).__init__(*args, **kwargs)
        #wsgi = kwargs['wsgi']
        #wsgi.register(RestVtepController, {RestVtep.__name__: self})

        # EvpnSpeaker instance instantiated later
        self.speaker = None

        # OVSBridge instance instantiated later
        self.ovs = None

        # Dictionary for retrieving the EvpnNetwork instance by VNI
        # self.networks = {
        #     <vni>: <instance 'EvpnNetwork'>,
        #     ...
        # }
        self.networks = {}
        self.cmd_dpid = 'ovs-vsctl get Bridge br-int datapath_id'
        self.cmd_lsp_add = 'ovn-nbctl lsp-add {} {}'
        self.cmd_lsp_del = 'ovn-nbctl lsp-del {} {}'
        self.cmd_lsp_get = 'ovn-nbctl -f json get Logical_Switch_Port {} addresses'
        self.cmd_lsp_set_addr = 'ovn-nbctl lsp-set-addresses {}'
        self.cmd_lsp_set_sec = 'ovn-nbctl lsp-set-port-security {}'
        self.cmd_set_manager_ovsdb = 'ovs-vsctl set-manager ptcp:{}'

    def _get_datapath(self):
        return ssh_command(
            hostname=DATAPATH_ADDR,
            username=USER,
            password=PASSWORD,
            port=PORT_SSH,
            cmd=self.cmd_dpid)

    # Utility methods related to OVSDB

    # Set manager-ovsdb
    def _set_manager_ovsdb(self):
        return ssh_command(
            hostname=DATAPATH_ADDR,
            username=USER,
            password=PASSWORD,
            port=PORT_SSH,
            cmd=self.cmd_set_manager_ovsdb.format(OVSDB_PORT))

    def _get_ovs_bridge(self):
        datapath = self._get_datapath()
        if datapath['err']:
            self.logger.debug(datapath['err'])
            return None
        dpid = to_int(datapath['out'], 16)

        ovsdb_addr = 'tcp:%s:%d' % (DATAPATH_ADDR, OVSDB_PORT)
        if (self.ovs is not None
                and self.ovs.datapath_id == dpid
                and self.ovs.vsctl.remote == ovsdb_addr):
            return self.ovs

        try:
            self.ovs = ovs_bridge.OVSBridge(
                CONF=self.CONF,
                datapath_id=dpid,
                ovsdb_addr=ovsdb_addr)
            self.ovs.init()
        except Exception as e:
            self.logger.exception('Cannot initiate OVSDB connection: %s', e)
            return None

        return self.ovs

    def _get_vxlan_port(self, remote_ip, key):
        # Searches VXLAN port named 'vxlan_<remote_ip>_<key>'
        ovs = self._get_ovs_bridge()
        if ovs is None:
            return None
        ports_ovs = ovs.get_port_name_list()
        port_vxlan_name = 'vxlan_%s_%s' % (remote_ip, key)
        if port_vxlan_name in ports_ovs:
            return port_vxlan_name
        else:
            return None

    def _add_vxlan_port(self, remote_ip, key):
        # If VXLAN port already exists, returns OFPort number
        vxlan_port = self._get_vxlan_port(remote_ip, key)
        if vxlan_port is not None:
            return vxlan_port

        ovs = self._get_ovs_bridge()
        if ovs is None:
            return None

        # Adds VXLAN port named 'vxlan_<remote_ip>_<key>'
        vxlan_port_name = 'vxlan_%s_%s' % (remote_ip, key)
        ovs.add_vxlan_port(
            name=vxlan_port_name,
            remote_ip=remote_ip,
            key=key)

        # Create logical port named 'vxlan_<remote_ip>_<key>' as physical port
        cmd = self.cmd_lsp_add.format(
            self.networks[key].logical_switch, vxlan_port_name
        )
        # TODO check operation
        ssh_command(
            hostname=OVNCENTR_ADDR,
            username=USER,
            password=PASSWORD,
            port=PORT_SSH,
            cmd=cmd)

        # Mapping OVS physical and logical VXLAN port
        # TODO check operation
        ovs.set_db_attribute("Interface", vxlan_port_name, "external_ids", "iface-id", key=vxlan_port_name)

        # Return VXLAN port name
        vxlan_port = self._get_vxlan_port(remote_ip, key)
        if vxlan_port:
            return vxlan_port_name
        else:
            return None

    def _del_vxlan_port(self, remote_ip, key):
        ovs = self._get_ovs_bridge()
        if ovs is None:
            return None

        # If VXLAN port does not exist, returns None
        vxlan_port = self._get_vxlan_port(remote_ip, key)
        if vxlan_port is None:
            return None

        # Delete physical and logical VXLAN port named 'vxlan_<remote_ip>_<key>'
        vxlan_port_name = 'vxlan_%s_%s' % (remote_ip, key)
        cmd = self.cmd_lsp_del.format(
            self.networks[key].logical_switch, vxlan_port_name
        )
        # TODO check operation
        ssh_command(
            hostname=OVNCENTR_ADDR,
            username=USER,
            password=PASSWORD,
            port=PORT_SSH,
            cmd=cmd)

        # TODO check operation
        ovs.remove_db_attribute("Interface", vxlan_port_name, "external_ids", "iface-id", key=vxlan_port_name)
        ovs.del_port(vxlan_port_name)

        # Returns deleted VXLAN port number
        return vxlan_port

    # Event handlers for BGP

    def _evpn_mac_ip_adv_route_handler(self, ev):
        network = self.networks.get(ev.path.nlri.vni, None)
        if network is None:
            self.logger.debug('No such VNI registered: %s', ev.path.nlri)
            return

        datapath = self._get_datapath()
        if datapath['err']:
            self.logger.debug(datapath['err'])
            return

        vxlan_port = self._add_vxlan_port(
            remote_ip=ev.nexthop,
            key=ev.path.nlri.vni)
        if vxlan_port is None:
            self.logger.debug('Cannot create a new VXLAN port: %s',
                              'vxlan_%s_%s' % (ev.nexthop, ev.path.nlri.vni))
            return

        # TODO update Logical VXLAN port
        # Get addresses from Logical VXLAN port
        cmd = self.cmd_lsp_get.format(vxlan_port)
        # TODO check operation
        res = ssh_command_json(
            hostname=OVNCENTR_ADDR,
            username=USER,
            password=PASSWORD,
            port=PORT_SSH,
            cmd=cmd)

        if res['err']:
            return
        addresses = res['out']
        address = ' '.join([ev.path.nlri.mac_addr, ev.path.nlri.ip_addr])
        if address in addresses:
            return
        addresses.append(address)

        # Set addresses on Logical VXLAN port
        cmd = self.cmd_lsp_set_addr.format(vxlan_port)
        for address in addresses:
            cmd += ' "' + address + '"'
        # TODO check operation
        res = ssh_command(
            hostname=OVNCENTR_ADDR,
            username=USER,
            password=PASSWORD,
            port=PORT_SSH,
            cmd=cmd)

        # Set security on Logical VXLAN port
        cmd = self.cmd_lsp_set_sec.format(vxlan_port, *addresses)
        # TODO check operation
        ssh_command(
            hostname=OVNCENTR_ADDR,
            username=USER,
            password=PASSWORD,
            port=PORT_SSH,
            cmd=cmd)

    def _evpn_incl_mcast_etag_route_handler(self, ev):
        # Note: For the VLAN Based service, we use RT(=RD) assigned
        # field as vid.
        vni = _RouteDistinguisher.from_str(ev.path.nlri.route_dist).assigned

        network = self.networks.get(vni, None)
        if network is None:
            self.logger.debug('No such VNI registered: %s', vni)
            return

        datapath = self._get_datapath()
        if datapath['err']:
            self.logger.debug(datapath['err'])
            return

        vxlan_port = self._add_vxlan_port(
            remote_ip=ev.nexthop,
            key=vni)
        if vxlan_port is None:
            self.logger.debug('Cannot create a new VXLAN port: %s',
                              'vxlan_%s_%s' % (ev.nexthop, vni))
            return

    def _evpn_route_handler(self, ev):
        if ev.path.nlri.type == EvpnNLRI.MAC_IP_ADVERTISEMENT:
            self._evpn_mac_ip_adv_route_handler(ev)
        elif ev.path.nlri.type == EvpnNLRI.INCLUSIVE_MULTICAST_ETHERNET_TAG:
            self._evpn_incl_mcast_etag_route_handler(ev)

    def _evpn_withdraw_mac_ip_adv_route_handler(self, ev):
        network = self.networks.get(ev.path.nlri.vni, None)
        if network is None:
            self.logger.debug('No such VNI registered: %s', ev.path.nlri)
            return

        datapath = self._get_datapath()
        if datapath['err']:
            self.logger.debug(datapath['err'])
            return

        vxlan_port = self._add_vxlan_port(
            remote_ip=ev.nexthop,
            key=ev.path.nlri.vni)
        if vxlan_port is None:
            self.logger.debug('there is no VXLAN port: %s',
                              'vxlan_%s_%s' % (ev.nexthop, ev.path.nlri.vni))
            return

        # TODO update Logical VXLAN port
        # Get addresses from Logical VXLAN port
        cmd = self.cmd_lsp_get.format(vxlan_port)
        # TODO check operation
        res = ssh_command(
            hostname=OVNCENTR_ADDR,
            username=USER,
            password=PASSWORD,
            port=PORT_SSH,
            cmd=cmd)
        if res['err']:
            return
        addresses = list(res['out'])
        address = ' '.join([ev.path.nlri.mac_addr, ev.path.nlri.ip_addr])
        if address not in addresses:
            return
        addresses.remove(address)

        # Set addresses on Logical VXLAN port
        cmd = self.cmd_lsp_set_addr.format(vxlan_port, *addresses)
        # TODO check operation
        ssh_command(
            hostname=OVNCENTR_ADDR,
            username=USER,
            password=PASSWORD,
            port=PORT_SSH,
            cmd=cmd)

        # Set security on Logical VXLAN port
        cmd = self.cmd_lsp_set_sec.format(vxlan_port, *addresses)
        # TODO check operation
        ssh_command(
            hostname=OVNCENTR_ADDR,
            username=USER,
            password=PASSWORD,
            port=PORT_SSH,
            cmd=cmd)

        client = network.clients.get(ev.path.nlri.mac_addr, None)
        if client is None:
            self.logger.debug('No such client: %s', ev.path.nlri.mac_addr)
            return

        network.clients.pop(ev.path.nlri.mac_addr)

    def _evpn_withdraw_incl_mcast_etag_route_handler(self, ev):
        # Note: For the VLAN Based service, we use RT(=RD) assigned
        # field as vid.
        vni = _RouteDistinguisher.from_str(ev.path.nlri.route_dist).assigned
        # vni = int(ev.path.nlri.route_dist.split(':')[1])

        network = self.networks.get(vni, None)
        if network is None:
            self.logger.debug('No such VNI registered: %s', vni)
            return

        datapath = self._get_datapath()
        # Check the datapath 'br-int'
        if datapath['err']:
            self.logger.debug(datapath['err'])
            if datapath['out'] is None:
                return {'DatapathNotFound': dict(datapath='br-int')}

        vxlan_port = self._get_vxlan_port(
            remote_ip=ev.nexthop,
            key=vni)
        if vxlan_port is None:
            self.logger.debug('No such VXLAN port: %s',
                              'vxlan_%s_%s' % (ev.nexthop, vni))
            return

        vxlan_port = self._del_vxlan_port(
            remote_ip=ev.nexthop,
            key=vni)
        if vxlan_port is None:
            self.logger.debug('Cannot delete VXLAN port: %s',
                              'vxlan_%s_%s' % (ev.nexthop, vni))
            return

    def _evpn_withdraw_route_handler(self, ev):
        if ev.path.nlri.type == EvpnNLRI.MAC_IP_ADVERTISEMENT:
            self._evpn_withdraw_mac_ip_adv_route_handler(ev)
        elif ev.path.nlri.type == EvpnNLRI.INCLUSIVE_MULTICAST_ETHERNET_TAG:
            self._evpn_withdraw_incl_mcast_etag_route_handler(ev)

    def _best_path_change_handler(self, ev):
        if not isinstance(ev.path, EvpnPath):
            # Ignores non-EVPN routes
            return
        elif ev.nexthop == self.speaker.router_id:
            # Ignore local connected routes
            return
        elif ev.is_withdraw:
            self._evpn_withdraw_route_handler(ev)
        else:
            self._evpn_route_handler(ev)

    def _peer_down_handler(self, remote_ip, remote_as):
        neighbor = self.speaker.neighbors.get(remote_ip, None)
        if neighbor is None:
            self.logger.debug('No such neighbor: remote_ip=%s, remote_as=%s',
                              remote_ip, remote_as)
            return

        neighbor.state = 'down'

    def _peer_up_handler(self, remote_ip, remote_as):
        neighbor = self.speaker.neighbors.get(remote_ip, None)
        if neighbor is None:
            self.logger.debug('No such neighbor: remote_ip=%s, remote_as=%s',
                              remote_ip, remote_as)
            return

        neighbor.state = 'up'

    # API methods for REST controller

    def add_speaker(self, ctx, arg):
        as_number = arg['as_number']
        router_id = arg['router_id']
        datapath = self._get_datapath()
        # Check the datapath 'br-int'
        if datapath['err']:
            self.logger.debug(datapath['err'])
            # TODO return exception
        if datapath['out'] is 'null':
            return {'DatapathNotFound': dict(datapath='br-int')}
        dpid = to_int(datapath['out'], 16)

        # TODO check exception
        self._set_manager_ovsdb()

        self.speaker = EvpnSpeaker(
            dpid=dpid,
            as_number=as_number,
            router_id=str(router_id),
            best_path_change_handler=self._best_path_change_handler,
            peer_down_handler=self._peer_down_handler,
            peer_up_handler=self._peer_up_handler)

        return {self.speaker.router_id: self.speaker.to_jsondict()}

    def get_speaker(self):
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        return {self.speaker.router_id: self.speaker.to_jsondict()}

    def del_speaker(self):
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        for vni in list(self.networks.keys()):
            arg = {'vni': vni}
            self.del_network(arg=arg)

        for address in list(self.speaker.neighbors.keys()):
            arg = {'address': address}
            self.del_neighbor(arg=arg)

        self.speaker.shutdown()
        speaker = self.speaker
        self.speaker = None

        return {speaker.router_id: speaker.to_jsondict()}

    def add_neighbor(self, ctx, arg):
        self.logger.debug(arg)
        self.logger.debug(type(arg))
        address = str(arg['address'])
        remote_as = arg['remote_as']
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        self.speaker.neighbor_add(
            address=address,
            remote_as=remote_as,
            enable_evpn=True)

        neighbor = EvpnNeighbor(
            address=address,
            remote_as=remote_as)
        self.speaker.neighbors[address] = neighbor

        return {address: neighbor.to_jsondict()}

    def get_neighbors(self, ctx, arg):
        address = arg.get('address')
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        if address is not None:
            address = str(address)
            neighbor = self.speaker.neighbors.get(address, None)
            if neighbor is None:
                return {'NeighborNotFound': dict(address=address)}
            return {address: neighbor.to_jsondict()}

        neighbors = {}
        for address, neighbor in self.speaker.neighbors.items():
            neighbors[address] = neighbor.to_jsondict()

        return neighbors

    def del_neighbor(self, ctx, arg):
        address = str(arg['address'])
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        neighbor = self.speaker.neighbors.get(address, None)
        if neighbor is None:
            return {'NeighborNotFound': dict(address=address)}

        for network in self.networks.values():
            for mac, client in list(network.clients.items()):
                if client.next_hop == address:
                    network.clients.pop(mac)

        self.speaker.neighbor_del(address=address)

        neighbor = self.speaker.neighbors.pop(address)

        return {address: neighbor.to_jsondict()}

    def add_network(self, ctx, arg):
        vni = arg['vni']
        network_id = arg['network_id']
        logical_switch = 'neutron-{}'.format(network_id)
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        # Constructs type 0 RD with as_number and vni
        route_dist = "%s:%d" % (self.speaker.as_number, vni)

        self.speaker.vrf_add(
            route_dist=route_dist,
            import_rts=[route_dist],
            export_rts=[route_dist],
            route_family=RF_L2_EVPN)

        # Note: For the VLAN Based service, ethernet_tag_id
        # must be set to zero.
        self.speaker.evpn_prefix_add(
            route_type=EVPN_MULTICAST_ETAG_ROUTE,
            route_dist=route_dist,
            ethernet_tag_id=vni,
            ip_addr=self.speaker.router_id,
            next_hop=self.speaker.router_id)

        network = EvpnNetwork(
            vni=vni,
            logical_switch=logical_switch,
            route_dist=route_dist,
            ethernet_tag_id=0)

        self.networks[vni] = network

        return {vni: network.to_jsondict()}

    def get_networks(self, ctx, arg):
        vni = arg.get['vni']
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        if vni is not None:
            network = self.networks.get(vni, None)
            if network is None:
                return {'VniNotFound': dict(vni=vni)}
            return {vni: network.to_jsondict()}

        networks = {}
        for vni, network in self.networks.items():
            networks[vni] = network.to_jsondict()

        return networks

    def del_network(self, ctx, arg):
        vni = arg.get['vni']
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        datapath = self._get_datapath()
        # Check the datapath 'br-int'
        if datapath['err']:
            self.logger.debug(datapath['err'])
            if datapath['out'] is None:
                return {'DatapathNotFound': dict(datapath='br-int')}

        network = self.networks.get(vni, None)
        if network is None:
            return {'VniNotFound': dict(vni=vni)}

        for client in network.get_clients(next_hop=self.speaker.router_id):
            self.del_client(
                ctx={},
                arg={'vni': vni, 'mac': client.mac}
            )

        for address in self.speaker.neighbors:
            self._del_vxlan_port(
                remote_ip=address,
                key=vni)

        self.speaker.evpn_prefix_del(
            route_type=EVPN_MULTICAST_ETAG_ROUTE,
            route_dist=network.route_dist,
            ethernet_tag_id=vni,
            ip_addr=self.speaker.router_id)

        self.speaker.vrf_del(route_dist=network.route_dist)

        network = self.networks.pop(vni)

        return {vni: network.to_jsondict()}

    def add_client(self, ctx, arg):
        vni = arg.get('vni')
        port = str(arg.get('port'))
        mac = str(arg.get('mac'))
        ip = str(arg.get('ip'))
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        datapath = self._get_datapath()
        # Check the datapath 'br-int'
        if datapath['err']:
            self.logger.debug(datapath['err'])
            if datapath['out'] is None:
                return {'DatapathNotFound': dict(datapath='br-int')}

        network = self.networks.get(vni, None)
        if network is None:
            return {'VniNotFound': dict(vni=vni)}

        # Note: For the VLAN Based service, ethernet_tag_id
        # must be set to zero.
        self.speaker.evpn_prefix_add(
            route_type=EVPN_MAC_IP_ADV_ROUTE,
            route_dist=network.route_dist,
            esi=0,
            ethernet_tag_id=0,
            mac_addr=mac,
            ip_addr=ip,
            vni=vni,
            next_hop=self.speaker.router_id,
            tunnel_type='vxlan')

        # Stores local client info
        client = EvpnClient(
            port=port,
            mac=mac,
            ip=ip,
            next_hop=self.speaker.router_id)
        network.clients[mac] = client

        self.logger.debug(client)

        return {vni: client.to_jsondict()}

    def del_client(self, ctx, arg):
        vni = arg.get['vni']
        mac = arg.get['mac']
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        datapath = self._get_datapath()
        # Check the datapath 'br-int'
        if datapath['err']:
            self.logger.debug(datapath['err'])
            if datapath['out'] is None:
                return {'DatapathNotFound': dict(datapath='br-int')}

        network = self.networks.get(vni, None)
        if network is None:
            return {'VniNotFound': dict(vni=vni)}

        client = network.clients.get(mac, None)
        if client is None:
            return {'ClientNotFound': dict(mac=mac)}
        elif client.next_hop != self.speaker.router_id:
            return {'ClientNotLocal': dict(mac=mac)}

        # Note: For the VLAN Based service, ethernet_tag_id
        # must be set to zero.
        self.speaker.evpn_prefix_del(
            route_type=EVPN_MAC_IP_ADV_ROUTE,
            route_dist=network.route_dist,
            esi=0,
            ethernet_tag_id=0,
            mac_addr=mac,
            ip_addr=client.ip)

        client = network.clients.pop(mac)

        return {vni: client.to_jsondict()}


##Create EndPoint List
endpoints = [RestVtep(), ]

##Create RPC Server
server = om.get_rpc_server(transport, target, endpoints, executor='eventlet')

##Start RPC Server
try:
    server.start()
    while True:
        time.sleep(1)
except KeyboardInterrupt:
    print("Stopping server")

server.stop()
server.wait()
