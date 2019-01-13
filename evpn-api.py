import json

from ryu.app.wsgi import ControllerBase
from ryu.app.wsgi import Response
from ryu.app.wsgi import route
from ryu.app.wsgi import WSGIApplication
from ryu.base import app_manager
from ryu.exception import RyuException

from vagrant.synced_folders_devstack.k8s.app_settings import RABBITMQ_SERVER, RABBIT_USER, RABBIT_PASSWORD

from oslo_config import cfg
import oslo_messaging as om


API_NAME = 'restvtep'


# Utility functions

def to_int(i):
    return int(str(i), 0)


def to_str_list(l):
    str_list = []
    for s in l:
        str_list.append(str(s))
    return str_list


# Exception classes related to OpenFlow and OVSDB

class RestApiException(RyuException):

    def to_response(self, status):
        body = {
            "error": str(self),
            "status": status,
        }
        return Response(content_type='application/json',
                        body=json.dumps(body), status=status)


class DatapathNotFound(RestApiException):
    message = 'No such datapath: %(datapath)s'


class OFPortNotFound(RestApiException):
    message = 'No such OFPort: %(port_name)s'


# Exception classes related to BGP

class BGPSpeakerNotFound(RestApiException):
    message = 'BGPSpeaker could not be found %(address)s'


class NeighborNotFound(RestApiException):
    message = 'No such neighbor: %(address)s'


class VniNotFound(RestApiException):
    message = 'No such VNI: %(vni)s'


class ClientNotFound(RestApiException):
    message = 'No such client: %(mac)s'


class ClientNotLocal(RestApiException):
    message = 'Specified client is not local: %(mac)s'


EXCEPTIONS = (DatapathNotFound, BGPSpeakerNotFound, NeighborNotFound, VniNotFound, ClientNotFound, ClientNotLocal)

class RestVtep(app_manager.RyuApp):
    #OFP_VERSIONS = [ofproto_v1_3.OFP_VERSION]
    _CONTEXTS = {'wsgi': WSGIApplication}

    def __init__(self, *args, **kwargs):
        super(RestVtep, self).__init__(*args, **kwargs)
        wsgi = kwargs['wsgi']
        wsgi.register(RestVtepController, {RestVtep.__name__: self})

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


def post_method(keywords):
    def _wrapper(method):
        def __wrapper(self, req, **kwargs):
            try:
                try:
                    body = req.json if req.body else {}
                except ValueError:
                    raise ValueError('Invalid syntax %s', req.body)
                kwargs.update(body)
                for key, converter in keywords.items():
                    value = kwargs.get(key, None)
                    if value is None:
                        raise ValueError('%s not specified' % key)
                    kwargs[key] = converter(value)
            except ValueError as e:
                return Response(content_type='application/json',
                                body={"error": str(e)}, status=400)
            try:
                return method(self, **kwargs)
            except Exception as e:
                status = 500
                body = {
                    "error": str(e),
                    "status": status,
                }
                return Response(content_type='application/json',
                                body=json.dumps(body), status=status)
        __wrapper.__doc__ = method.__doc__
        return __wrapper
    return _wrapper


def get_method(keywords=None):
    keywords = keywords or {}

    def _wrapper(method):
        def __wrapper(self, _, **kwargs):
            try:
                for key, converter in keywords.items():
                    value = kwargs.get(key, None)
                    if value is None:
                        continue
                    kwargs[key] = converter(value)
            except ValueError as e:
                return Response(content_type='application/json',
                                body={"error": str(e)}, status=400)
            try:
                return method(self, **kwargs)
            except Exception as e:
                status = 500
                body = {
                    "error": str(e),
                    "status": status,
                }
                return Response(content_type='application/json',
                                body=json.dumps(body), status=status)
        __wrapper.__doc__ = method.__doc__
        return __wrapper
    return _wrapper


delete_method = get_method


class RestVtepController(ControllerBase):

    def __init__(self, req, link, data, **config):
        super(RestVtepController, self).__init__(req, link, data, **config)
        self.vtep_app = data[RestVtep.__name__]
        self.logger = self.vtep_app.logger

        #Invoke "get_transport". This call will set default Configurations required to Create Messaging Transport
        self.transport = om.get_transport(cfg.CONF)

        cfg.CONF.set_override(
            'transport_url', 'rabbit://{}:{}@{}:5672//'.format(RABBIT_USER, RABBIT_PASSWORD, RABBITMQ_SERVER))

        #Create Messaging Transport
        self.transport = om.get_transport(cfg.CONF)

        #Create Target
        self.target = om.Target(topic='ovn_bus')

        #Create RPC Client
        self.client = om.RPCClient(self.transport, self.target)

        #Request context dict
        self.ctxt = {}

    @route(API_NAME, '/vtep/speakers', methods=['POST'])
    @post_method(
        keywords={
            "as_number": to_int,
            "router_id": str,
        })
    def add_speaker(self, **kwargs):
        """
        Creates a new BGPSpeaker instance.
        Usage:
            ======= ================
            Method  URI
            ======= ================
            POST    /vtep/speakers
            ======= ================
        Request parameters:
            ========== ============================================
            Attribute  Description
            ========== ============================================
            as_number  AS number. (e.g. 65000)
            router_id  Router ID. (e.g. "172.17.0.1")
            ========== ============================================
        Example::
            $ curl -X POST -d '{
             "as_number": 65000,
             "router_id": "172.17.0.1"
             }' http://localhost:8080/vtep/speakers | python -m json.tool
        ::
            {
                "172.17.0.1": {
                    "EvpnSpeaker": {
                        "as_number": 65000,
                        "neighbors": {},
                        "router_id": "172.17.0.1"
                    }
                }
            }
        """
        body = self.client.call(self.ctxt, 'add_speaker', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))

    @route(API_NAME, '/vtep/speakers', methods=['GET'])
    @get_method()
    def get_speakers(self, **kwargs):
        """
        Gets the info of BGPSpeaker instance.
        Usage:
            ======= ================
            Method  URI
            ======= ================
            GET     /vtep/speakers
            ======= ================
        Example::
            $ curl -X GET http://localhost:8080/vtep/speakers |
             python -m json.tool
        ::
            {
                "172.17.0.1": {
                    "EvpnSpeaker": {
                        "as_number": 65000,
                        "neighbors": {
                            "172.17.0.2": {
                                "EvpnNeighbor": {
                                    "address": "172.17.0.2",
                                    "remote_as": 65000,
                                    "state": "up"
                                }
                            }
                        },
                        "router_id": "172.17.0.1"
                    }
                }
            }
        """
        body = self.client.call(self.ctxt, 'get_speaker', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))

    @route(API_NAME, '/vtep/speakers', methods=['DELETE'])
    @delete_method()
    def del_speaker(self, **kwargs):
        """
        Shutdowns BGPSpeaker instance.
        Usage:
            ======= ================
            Method  URI
            ======= ================
            DELETE  /vtep/speakers
            ======= ================
        Example::
            $ curl -X DELETE http://localhost:8080/vtep/speakers |
             python -m json.tool
        ::
            {
                "172.17.0.1": {
                    "EvpnSpeaker": {
                        "as_number": 65000,
                        "neighbors": {},
                        "router_id": "172.17.0.1"
                    }
                }
            }
        """
        body = self.client.call(self.ctxt, 'del_speaker', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))

    @route(API_NAME, '/vtep/neighbors', methods=['POST'])
    @post_method(
        keywords={
            "address": str,
            "remote_as": to_int,
        })
    def add_neighbor(self, **kwargs):
        """
        Registers a new neighbor to the speaker.
        Usage:
            ======= ========================
            Method  URI
            ======= ========================
            POST    /vtep/neighbors
            ======= ========================
        Request parameters:
            ========== ================================================
            Attribute  Description
            ========== ================================================
            address    IP address of neighbor. (e.g. "172.17.0.2")
            remote_as  AS number of neighbor. (e.g. 65000)
            ========== ================================================
        Example::
            $ curl -X POST -d '{
             "address": "172.17.0.2",
             "remote_as": 65000
             }' http://localhost:8080/vtep/neighbors |
             python -m json.tool
        ::
            {
                "172.17.0.2": {
                    "EvpnNeighbor": {
                        "address": "172.17.0.2",
                        "remote_as": 65000,
                        "state": "down"
                    }
                }
            }
        """
        body = self.client.call(self.ctxt, 'add_neighbor', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))

    def _get_neighbors(self, **kwargs):
        body = self.client.call(self.ctxt, 'get_neighbors', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))

    @route(API_NAME, '/vtep/neighbors', methods=['GET'])
    @get_method()
    def get_neighbors(self, **kwargs):
        """
        Gets a list of all neighbors.
        Usage:
            ======= ========================
            Method  URI
            ======= ========================
            GET     /vtep/neighbors
            ======= ========================
        Example::
            $ curl -X GET http://localhost:8080/vtep/neighbors |
             python -m json.tool
        ::
            {
                "172.17.0.2": {
                    "EvpnNeighbor": {
                        "address": "172.17.0.2",
                        "remote_as": 65000,
                        "state": "up"
                    }
                }
            }
        """
        return self._get_neighbors(**kwargs)

    @route(API_NAME, '/vtep/neighbors/{address}', methods=['GET'])
    @get_method(
        keywords={
            "address": str,
        })
    def get_neighbor(self, **kwargs):
        """
        Gets the neighbor for the specified address.
        Usage:
            ======= ==================================
            Method  URI
            ======= ==================================
            GET     /vtep/neighbors/{address}
            ======= ==================================
        Request parameters:
            ========== ================================================
            Attribute  Description
            ========== ================================================
            address    IP address of neighbor. (e.g. "172.17.0.2")
            ========== ================================================
        Example::
            $ curl -X GET http://localhost:8080/vtep/neighbors/172.17.0.2 |
             python -m json.tool
        ::
            {
                "172.17.0.2": {
                    "EvpnNeighbor": {
                        "address": "172.17.0.2",
                        "remote_as": 65000,
                        "state": "up"
                    }
                }
            }
        """
        return self._get_neighbors(**kwargs)

    @route(API_NAME, '/vtep/neighbors/{address}', methods=['DELETE'])
    @delete_method(
        keywords={
            "address": str,
        })
    def del_neighbor(self, **kwargs):
        """
        Unregister the specified neighbor from the speaker.
        Usage:
            ======= ==================================
            Method  URI
            ======= ==================================
            DELETE  /vtep/speaker/neighbors/{address}
            ======= ==================================
        Request parameters:
            ========== ================================================
            Attribute  Description
            ========== ================================================
            address    IP address of neighbor. (e.g. "172.17.0.2")
            ========== ================================================
        Example::
            $ curl -X DELETE http://localhost:8080/vtep/speaker/neighbors/172.17.0.2 |
             python -m json.tool
        ::
            {
                "172.17.0.2": {
                    "EvpnNeighbor": {
                        "address": "172.17.0.2",
                        "remote_as": 65000,
                        "state": "up"
                    }
                }
            }
        """
        body = self.client.call(self.ctxt, 'del_neighbor', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))

    @route(API_NAME, '/vtep/networks', methods=['POST'])
    @post_method(
        keywords={
            "vni": to_int,
            "network_id": str,
        })
    def add_network(self, **kwargs):
        """
        Defines a new network.
        Usage:
            ======= ===============
            Method  URI
            ======= ===============
            POST    /vtep/networks
            ======= ===============
        Request parameters:
            ================ ========================================
            Attribute        Description
            ================ ========================================
            vni              Virtual Network Identifier. (e.g. 10)
            network_id       Neutron Network Identifier.
                             (e.g. 95c5a37c-4597-45cb-ba67-10a9c5aca3ba)
            ================ ========================================
        Example::
            $ curl -X POST -d '{
             "vni": 10,
             "network_id": 95c5a37c-4597-45cb-ba67-10a9c5aca3ba,
             }' http://localhost:8080/vtep/networks | python -m json.tool
        ::
            {
                "10": {
                    "EvpnNetwork": {
                        "clients": {},
                        "ethernet_tag_id": 0,
                        "route_dist": "65000:10",
                        "vni": 10,
                        "network_id": 95c5a37c-4597-45cb-ba67-10a9c5aca3ba
                    }
                }
            }
        """
        body = self.client.call(self.ctxt, 'add_network', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))

    def _get_networks(self, **kwargs):
        body = self.client.call(self.ctxt, 'get_networks', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))

    @route(API_NAME, '/vtep/networks', methods=['GET'])
    @get_method()
    def get_networks(self, **kwargs):
        """
        Gets a list of all networks.
        Usage:
            ======= ===============
            Method  URI
            ======= ===============
            GET     /vtep/networks
            ======= ===============
        Example::
            $ curl -X GET http://localhost:8080/vtep/networks |
             python -m json.tool
        ::
            {
                "10": {
                    "EvpnNetwork": {
                        "clients": {
                            "aa:bb:cc:dd:ee:ff": {
                                "EvpnClient": {
                                    "ip": "10.0.0.1",
                                    "mac": "aa:bb:cc:dd:ee:ff",
                                    "next_hop": "172.17.0.1",
                                    "port": "f9ba032a-0446-4e7c-b30b-454318e195b4"
                                }
                            }
                        },
                        "ethernet_tag_id": 0,
                        "route_dist": "65000:10",
                        "vni": 10
                    }
                }
            }
        """
        return self._get_networks(**kwargs)

    @route(API_NAME, '/vtep/networks/{vni}', methods=['GET'])
    @get_method(
        keywords={
            "vni": to_int,
        })
    def get_network(self, **kwargs):
        """
        Gets the network for the specified VNI.
        Usage:
            ======= =====================
            Method  URI
            ======= =====================
            GET     /vtep/networks/{vni}
            ======= =====================
        Request parameters:
            ================ ========================================
            Attribute        Description
            ================ ========================================
            vni              Virtual Network Identifier. (e.g. 10)
            ================ ========================================
        Example::
            $ curl -X GET http://localhost:8080/vtep/networks/10 |
             python -m json.tool
        ::
            {
                "10": {
                    "EvpnNetwork": {
                        "clients": {
                            "aa:bb:cc:dd:ee:ff": {
                                "EvpnClient": {
                                    "ip": "10.0.0.1",
                                    "mac": "aa:bb:cc:dd:ee:ff",
                                    "next_hop": "172.17.0.1",
                                    "port": "f9ba032a-0446-4e7c-b30b-454318e195b4"
                                }
                            }
                        },
                        "ethernet_tag_id": 0,
                        "route_dist": "65000:10",
                        "vni": 10
                    }
                }
            }
        """
        return self._get_networks(**kwargs)

    @route(API_NAME, '/vtep/networks/{vni}', methods=['DELETE'])
    @delete_method(
        keywords={
            "vni": to_int,
        })
    def del_network(self, **kwargs):
        """
        Deletes the network for the specified VNI.
        Usage:
            ======= =====================
            Method  URI
            ======= =====================
            DELETE  /vtep/networks/{vni}
            ======= =====================
        Request parameters:
            ================ ========================================
            Attribute        Description
            ================ ========================================
            vni              Virtual Network Identifier. (e.g. 10)
            ================ ========================================
        Example::
            $ curl -X DELETE http://localhost:8080/vtep/networks/10 |
             python -m json.tool
        ::
            {
                "10": {
                    "EvpnNetwork": {
                        "ethernet_tag_id": 10,
                        "clients": [
                            {
                                "EvpnClient": {
                                    "ip": "10.0.0.11",
                                    "mac": "e2:b1:0c:ba:42:ed",
                                    "port": "f9ba032a-0446-4e7c-b30b-454318e195b4"
                                }
                            }
                        ],
                        "route_dist": "65000:100",
                        "vni": 10
                    }
                }
            }
        """
        body = self.client.call(self.ctxt, 'del_network', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))

    @route(API_NAME, '/vtep/networks/{vni}/clients', methods=['POST'])
    @post_method(
        keywords={
            "vni": to_int,
            "port": str,
            "mac": str,
            "ip": str,
        })
    def add_client(self, **kwargs):
        """
        Registers a new client to the specified network.
        Usage:
            ======= =============================
            Method  URI
            ======= =============================
            POST    /vtep/networks/{vni}/clients
            ======= =============================
        Request parameters:
            =========== ===========================================================
            Attribute   Description
            =========== ===========================================================
            vni         Virtual Network Identifier. (e.g. 10)
            port        Logical Port name OVN. It corresponds with Port ID Neutron.
                        (e.g. "f9ba032a-0446-4e7c-b30b-454318e195b4")
            mac         Client MAC address to register.
                        (e.g. "aa:bb:cc:dd:ee:ff")
            ip          Client IP address. (e.g. "10.0.0.1")
            =========== ===========================================================
        Example::
            $ curl -X POST -d '{
             "port": "s1-eth1",
             "mac": "aa:bb:cc:dd:ee:ff",
             "ip": "10.0.0.1"
             }' http://localhost:8080/vtep/networks/10/clients |
             python -m json.tool
        ::
            {
                "10": {
                    "EvpnClient": {
                        "ip": "10.0.0.1",
                        "mac": "aa:bb:cc:dd:ee:ff",
                        "next_hop": "172.17.0.1",
                        "port": "f9ba032a-0446-4e7c-b30b-454318e195b4"
                    }
                }
            }
        """
        body = self.client.call(self.ctxt, 'add_client', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))

    @route(API_NAME, '/vtep/networks/{vni}/clients/{mac}', methods=['DELETE'])
    @delete_method(
        keywords={
            "vni": to_int,
            "mac": str,
        })
    def del_client(self, **kwargs):
        """
        Delete the client (with specified mac-address) from the specified network.
        Usage:
            ======= ===================================
            Method  URI
            ======= ===================================
            DELETE  /vtep/networks/{vni}/clients/{mac}
            ======= ===================================
        Request parameters:
            =========== ===============================================
            Attribute   Description
            =========== ===============================================
            vni         Virtual Network Identifier. (e.g. 10)
            mac         Client MAC address to register.
            =========== ===============================================
        Example::
            $ curl -X DELETE http://localhost:8080/vtep/networks/10/clients/aa:bb:cc:dd:ee:ff |
             python -m json.tool
        ::
            {
                "10": {
                    "EvpnClient": {
                        "ip": "10.0.0.1",
                        "mac": "aa:bb:cc:dd:ee:ff",
                        "next_hop": "172.17.0.1",
                        "port": "f9ba032a-0446-4e7c-b30b-454318e195b4"
                    }
                }
            }
        """
        body = self.client.call(self.ctxt, 'del_client', arg=kwargs)
        for e in EXCEPTIONS:
            exception = body.get(e.__name__)
            if exception:
                return e(**exception).to_response(status=404)

        return Response(content_type='application/json',
                        body=json.dumps(body))
