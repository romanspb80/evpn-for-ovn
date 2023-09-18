from ryu.exception import RyuException
from oslo_config import cfg
import oslo_messaging as om
from flask import Flask, request, jsonify
from flask_expects_json import expects_json
import copy
import sys
sys.path.append('/config')
from app_settings import RABBIT_USER, RABBIT_PASSWORD, RABBITMQ_SERVER


app = Flask(__name__)

# Invoke "get_transport". This call will set default Configurations required to Create Messaging Transport
transport = om.get_transport(cfg.CONF)

cfg.CONF.set_override(
    'transport_url', 'rabbit://{}:{}@{}:5672//'.format(RABBIT_USER, RABBIT_PASSWORD, RABBITMQ_SERVER))

# Create Messaging Transport
transport = om.get_transport(cfg.CONF)

# Create Target
target = om.Target(topic='bgpagent_bus')

# Create RPC Client
client = om.RPCClient(transport, target)

# Request context dict
ctxt = {}


class DatapathNotFound(RyuException):
    message = 'No such datapath: {}'


class BGPSpeakerNotFound(RyuException):
    message = 'No such BGPSpeaker: {}'


class NeighborNotFound(RyuException):
    message = 'No such neighbor: {}'


class VniNotFound(RyuException):
    message = 'No such VNI: {}'


class ClientNotFound(RyuException):
    message = 'No such client: {}'


class ClientNotLocal(RyuException):
    message = 'Specified client is not local: {}'


EXCEPTIONS = (DatapathNotFound, BGPSpeakerNotFound, NeighborNotFound, VniNotFound, ClientNotFound, ClientNotLocal)


schema = {
    "type": "object",
    "properties": {
        "as_number": {"type": "number"},
        "router_id": {"type": "string"},
        "address": {"type": "string"},
        "remote_as": {"type": "number"},
        "vni": {"type": "number"},
        "logical_switch": {"type": "string"},
        "mac": {"type": "string"},
        "ip": {"type": "string"}
    }
}


def required(required):
    _schema = copy.deepcopy(schema)
    _schema["required"] = required
    return _schema


def _handler(action, arg):
    body = client.call(ctxt, action, arg=arg)
    for e in EXCEPTIONS:
        exception = body.get(e.__name__)
        if exception:
            return jsonify({"error": e.message.format(**exception)}), 404

    return jsonify(body), 200


@app.route('/vtep/speakers', methods=['POST'])
@expects_json(schema=required(
    required=["as_number",
              "router_id"]))
def add_speaker():
    content = request.get_json()
    return _handler(action='add_speaker', arg=content)


@app.route('/vtep/speakers', methods=['GET'])
def get_speakers():
    return _handler(action='get_speaker', arg={})


@app.route('/vtep/speakers', methods=['DELETE'])
def del_speaker():
    return _handler(action='del_speaker', arg={})


@app.route('/vtep/neighbors', methods=['POST'])
@expects_json(schema=required(
    required=["address",
              "remote_as"]))
def add_neighbor():
    content = request.get_json()
    return _handler(action='add_neighbor', arg=content)


def _get_neighbors(content):
    return _handler(action='get_neighbors', arg=content)


@app.route('/vtep/neighbors', methods=['GET'])
def get_neighbors():
    content = {}
    return _get_neighbors(content)


@app.route('/vtep/neighbors/<address>', methods=['GET'])
def get_neighbor(address):
    content = {"address": address}
    return _get_neighbors(content)


@app.route('/vtep/neighbors/<address>', methods=['DELETE'])
def del_neighbor(address):
    content = {"address": address}
    return _handler(action='del_neighbor', arg=content)


@app.route('/vtep/networks', methods=['POST'])
@expects_json(schema=required(
    required=["vni"]))
def add_network():
    content = request.get_json()
    return _handler(action='add_network', arg=content)


def _get_networks(content):
    return _handler(action='get_networks', arg=content)


@app.route('/vtep/networks', methods=['GET'])
def get_networks():
    content = {}
    return _get_networks(content)


@app.route('/vtep/networks/<vni>', methods=['GET'])
def get_network(vni):
    content = {"vni": vni}
    return _get_networks(content)


@app.route('/vtep/networks/<vni>', methods=['DELETE'])
def del_network(vni):
    content = {"vni": vni}
    return _handler(action='del_network', arg=content)


@app.route('/vtep/networks/<vni>/clients', methods=['POST'])
@expects_json(schema=required(
    required=["port",
              "mac",
              "ip"]))
def add_client(vni):
    content = request.get_json()
    content.update({"vni": vni})
    return _handler(action='add_client', arg=content)


@app.route('/vtep/networks/<vni>/clients/<mac>', methods=['DELETE'])
def del_client(vni, mac):
    content = {
        "vni": vni,
        "mac": mac
    }
    return _handler(action='del_client', arg=content)

@app.route("/test")
def hello_world():
    return "<p>Hello, World!</p>"


if __name__ == '__main__':
    app.run(debug=True, host='0.0.0.0')
