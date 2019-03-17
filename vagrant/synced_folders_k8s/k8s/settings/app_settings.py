# Settings for RabbitMQ Server connection (DEVSTACK)
RABBITMQ_SERVER='rabbitmq-svc'
RABBIT_USER='stackrabbit'
RABBIT_PASSWORD='password'


# Settings for SSH connection to host with Neutron (DEVSTACK)
USER='root'
PASSWORD='password'
PORT_SSH=22

# Network Node IP address (OVS bridge)
DATAPATH_ADDR='192.168.10.200'
# Neutron Server (OVN Central) IP address
OVNCENTR_ADDR='192.168.10.200'

# IP address of the External system
RYU_ADDR='192.168.10.10'