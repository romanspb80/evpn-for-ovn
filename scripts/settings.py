# OVN address
SERVER_ADDRESS = '192.168.10.20'

# OVSDB connections
OVSDB_OVS_CONN = 'tcp:{}:6640'.format(SERVER_ADDRESS)
OVSDB_OVNNB_CONN = 'tcp:{}:6641'.format(SERVER_ADDRESS)

# OVS Bridge for client connections
OVS_BRIDGE = 'br-int'

# Client settings
OVS_PORT = 'vm1'
OVN_SWITCH = 'ls1'
LSP = 'ls1-vm1'
VM1_IP = '192.168.222.12/24'
VM1_MAC = '02:ac:10:ff:00:12'
NETNS1 = OVS_PORT





