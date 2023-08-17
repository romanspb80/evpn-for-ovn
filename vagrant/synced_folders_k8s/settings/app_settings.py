# Devstack address
OVNCLUSTER_ADDRESS = '192.168.10.20'

# Settings for RabbitMQ Server connection (DEVSTACK)
RABBITMQ_SERVER = OVNCLUSTER_ADDRESS
RABBIT_USER = 'stackrabbit'
RABBIT_PASSWORD = 'secret'

# Settings for SSH connection to host with Neutron (DEVSTACK)
USER = 'vagrant'
PASSWORD = 'vagrant'
PORT_SSH = 22

# OVSDB connections
OVSDB_OVS_CONN = 'tcp:{}:6640'.format(OVNCLUSTER_ADDRESS)
OVSDB_OVNNB_CONN = 'tcp:{}:6641'.format(OVNCLUSTER_ADDRESS)

# OVS Bridge for client connections
OVS_NAME = 'br-int'
