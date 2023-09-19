from ryu.base import app_manager
from ryu.lib.packet.bgp import _RouteDistinguisher
from ryu.lib.packet.bgp import EvpnNLRI
from ryu.lib.stringify import StringifyMixin
from ryu.services.protocols.bgp.bgpspeaker import BGPSpeaker
from ryu.services.protocols.bgp.bgpspeaker import RF_L2_EVPN
from ryu.services.protocols.bgp.bgpspeaker import EVPN_MAC_IP_ADV_ROUTE
from ryu.services.protocols.bgp.bgpspeaker import EVPN_MULTICAST_ETAG_ROUTE
from ryu.services.protocols.bgp.info_base.evpn import EvpnPath

import logging
import sys
sys.path.append('/config')
from app_settings import RABBITMQ_SERVER, RABBIT_USER, RABBIT_PASSWORD

from oslo_config import cfg
import oslo_messaging as om
import time
import eventlet
eventlet.monkey_patch()

logger = logging.getLogger(__name__)

EXCEPTIONS = []

# Invoke "get_transport". This call will set default Configurations required to Create Messaging Transport
transport = om.get_transport(cfg.CONF)

cfg.CONF.set_override(
    'transport_url', 'rabbit://{}:{}@{}:5672//'.format(RABBIT_USER, RABBIT_PASSWORD, RABBITMQ_SERVER))

# Create Messaging Transport
transport = om.get_transport(cfg.CONF)

# Request context dict
ctxt = {}


# Create MP-BGP agent Target (Exchange, Topic and Server to listen on)
target_bgpagent = om.Target(topic='bgpagent_bus', server=RABBITMQ_SERVER)

# Create OVN Target (Exchange, Topic and Server to listen on)
target_ovn = om.Target(topic='ovn_bus', server=RABBITMQ_SERVER)

# Create RPC Client
client_ovn = om.RPCClient(transport, target_ovn)


def to_int(i, base):
    return int(str(i), base)


class EvpnSpeaker(BGPSpeaker, StringifyMixin):
    _TYPE = {
        'ascii': [
            'router_id',
        ],
    }

    def __init__(self, as_number, router_id,
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


class RestVtep(app_manager.RyuApp):
    def __init__(self, *args, **kwargs):
        super(RestVtep, self).__init__(*args, **kwargs)
        self.speaker = None

        # OVSBridge instance instantiated later
        self.ovs = None

        # Dictionary for retrieving the EvpnNetwork instance by VNI
        # self.networks = {
        #     <vni>: <instance 'EvpnNetwork'>,
        #     ...
        # }
        self.networks = {}

        self.logger = logger

    def _handler(self, action, arg):
        body = client_ovn.call(ctxt, action, **arg)
        # for e in EXCEPTIONS:
        #     exception = body.get(e.__name__)
        #     if exception:
        #         return {"error": e.message.format(**exception)}

        return body

    def _evpn_mac_ip_adv_route_handler(self, ev):
        network = self.networks.get(ev.path.nlri.vni, None)
        if network is None:
            self.logger.debug('No such VNI registered: {}'.format(ev.path.nlri))
            return

        content = {"remote_ip": ev.nexthop, "key": ev.path.nlri.vni, "logical_switch": network.logical_switch}
        vxlan_port = self._handler(action='add_vxlan_port', arg=content)
        if vxlan_port is None:
            self.logger.debug('Cannot create a new VXLAN port: {}'.format(
                'vxlan_{}_{}'.format(ev.nexthop, ev.path.nlri.vni)))
            return

        address = ' '.join([ev.path.nlri.mac_addr, ev.path.nlri.ip_addr])
        content = {"port": vxlan_port, "address": address, "action": "add"}
        self._handler(action='update_lsp_addr', arg=content)

        network.clients[ev.path.nlri.mac_addr] = EvpnClient(
            port=vxlan_port,
            mac=ev.path.nlri.mac_addr,
            ip=ev.path.nlri.ip_addr,
            next_hop=ev.nexthop)

    def _evpn_incl_mcast_etag_route_handler(self, ev):
        # Note: For the VLAN Based service, we use RT(=RD) assigned
        # field as vid.
        vni = _RouteDistinguisher.from_str(ev.path.nlri.route_dist).assigned

        network = self.networks.get(vni, None)
        if network is None:
            self.logger.debug('No such VNI registered: %s', vni)
            return

        content = {"remote_ip": ev.nexthop, "key": vni, "logical_switch": network.logical_switch}
        vxlan_port = self._handler(action='add_vxlan_port', arg=content)
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

        content = {"remote_ip": ev.nexthop, "key": ev.path.nlri.vni, "logical_switch": network.logical_switch}
        vxlan_port = self._handler(action='add_vxlan_port', arg=content)
        if vxlan_port is None:
            self.logger.debug('there is no VXLAN port: %s',
                              'vxlan_%s_%s' % (ev.nexthop, ev.path.nlri.vni))
            return

        address = ' '.join([ev.path.nlri.mac_addr, ev.path.nlri.ip_addr])
        content = {"port": vxlan_port, "address": address, "action": "add"}
        self._handler(action='update_lsp_addr', arg=content)

        client = network.clients.get(ev.path.nlri.mac_addr, None)
        if client is None:
            self.logger.debug('No such client: {}'.format(ev.path.nlri.mac_addr))
            return

        network.clients.pop(ev.path.nlri.mac_addr)

    def _evpn_withdraw_incl_mcast_etag_route_handler(self, ev):
        # Note: For the VLAN Based service, we use RT(=RD) assigned
        # field as vid.
        vni = _RouteDistinguisher.from_str(ev.path.nlri.route_dist).assigned
        # vni = int(ev.path.nlri.route_dist.split(':')[1])

        network = self.networks.get(vni, None)
        if network is None:
            self.logger.debug('No such VNI registered: {}'.format(vni))
            return

        content = {"remote_ip": ev.nexthop, "key": vni}
        vxlan_port = self._handler(action='get_vxlan_port', arg=content)
        if vxlan_port is None:
            self.logger.debug('No such VXLAN port: {}'.format('vxlan_{}_{}'.format(ev.nexthop, vni)))
            return

        vxlan_port = self._handler(action='del_vxlan_port', arg=content)
        if vxlan_port is None:
            self.logger.debug('Cannot delete VXLAN port: {}'.format('vxlan_{}_{}'.format(ev.nexthop, vni)))
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

        self.speaker = EvpnSpeaker(
            as_number=as_number,
            router_id=str(router_id),
            best_path_change_handler=self._best_path_change_handler,
            peer_down_handler=self._peer_down_handler,
            peer_up_handler=self._peer_up_handler)

        return {self.speaker.router_id: self.speaker.to_jsondict()}

    def get_speaker(self, ctx, arg):
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        return {self.speaker.router_id: self.speaker.to_jsondict()}

    def del_speaker(self, ctx, arg):
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
        logical_switch = arg['logical_switch']
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        # Constructs type 0 RD with as_number and vni
        route_dist = "{}:{}".format(self.speaker.as_number, vni)

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
        vni = arg.get('vni')
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        if vni is not None:
            vni = to_int(vni, 10)
            network = self.networks.get(vni, None)
            if network is None:
                return {'VniNotFound': dict(vni=vni)}
            return {vni: network.to_jsondict()}

        networks = {}
        for vni, network in self.networks.items():
            networks[vni] = network.to_jsondict()

        return networks

    def del_network(self, ctx, arg):
        vni = to_int(arg.get('vni'), 10)
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

        network = self.networks.get(vni, None)
        if network is None:
            return {'VniNotFound': dict(vni=vni)}

        for client in network.get_clients(next_hop=self.speaker.router_id):
            self.del_client(
                ctx={},
                arg={'vni': vni, 'mac': client.mac}
            )

        for address in self.speaker.neighbors:
            content = {"remote_ip": address, "key": vni}
            vxlan_port = self._handler(action='del_vxlan_port', arg=content)
            if vxlan_port is None:
                self.logger.debug('Cannot delete VXLAN port: {}'.format('vxlan_{}_{}'.format(address, vni)))
                return

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
        vni = to_int(vni, 10)
        port = str(arg.get('port'))
        mac = str(arg.get('mac'))
        ip = str(arg.get('ip'))
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

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
        vni = arg.get('vni')
        vni = to_int(vni, 10)
        mac = arg.get('mac')
        if self.speaker is None:
            return {'BGPSpeakerNotFound': dict(address='')}

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
server = om.get_rpc_server(transport, target_bgpagent, endpoints, executor='threading')

##Start RPC Server
try:
    server.start()
    while True:
        time.sleep(1)
except KeyboardInterrupt:
    print("Stopping server")

server.stop()
server.wait()
