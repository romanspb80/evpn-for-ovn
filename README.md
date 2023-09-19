# evpn-for-ovn
A prototype of EVPN-VXLAN implementation for network cloud solution based on OVN.
# Introduction
OVN is described in the [document](https://www.ovn.org/support/dist-docs/ovn-architecture.7.txt):

"OVN, the Open Virtual Network, is a system to support virtual network abstraction. OVN complements the existing capabilities of OVS to add native support for virtual network abstractions, such as virtual L2 and L3 overlays and security groups. Services such as DHCP are also  desirable  features. Just like OVS, OVN’s design goal is to have a production-quality implementation that can operate at significant scale."

And the main purpose of OVN is a Control Plane constructing for Neutron OpenStack. Also there are some solutions for Kubernetes networking model implementation: Kube-OVN, OVN4NFV-K8s-Plugin and OVN-Kubernetes.
OVN is built on the same architectural principles as VMware's commercial NSX and offers the same core network virtualization capability — providing a free alternative that has been adopted in open source orchestration systems. But OVN can not implement EVPN Multi-Site architecture for DCI (Data Center Interconnect) concept.

This project demonstrates how it can be solved with adding new applications of RYU. RYU is a SDN framework with the libraries of different network protocols and written in Python: https://ryu.readthedocs.io/en/latest/getting_started.html#what-s-ryu

# How it works
Assume that there are two data centers - DataCenter A and DataCenter B. And needs to organize a EVPN connection between them. DataCenter A is an External System with EVPN-VXLAN support. In the project the built-in RYU-application **rest_vtep.py**  emulates the EVPN solution for External System. DataCenter B is our Cloud Platform based on OpenStack with OVN.
The diagram shows the following elements.
1. DataCenter A (an External System) with the RYU-application rest_vtep.py
2. DataCenter B includes:
- Cloud Platform based on OVN networking (eg. Openstack with Neutron OVN ML2 plugin)
- K8S Cluster (Microk8s) with our applications for OVN EVPN-VXLAN implementation.
- Some kind of an Orchestrator or [Cloud Management System](https://en.wikipedia.org/wiki/Cloud_management) (CMS).
![evpn_4_ovn](https://github.com/romanspb80/evpn-for-ovn/assets/30826451/10cc960d-7e22-457c-89bd-8d2fe223dfce)
## Idea description
Needs to organize a L2VPN (distributed L2 network) between sites (Data Centers) using EVPN-VXLAN . One of them is our Cloud Platform with OVN - DataCenter B. Another is external system with EVPN-VXLAN support - DataCenter A. The key step is the implementation of EVPN-VXLAN on Neutron side. To do this, it is necessary to implement the building vxlan tunnels as a Data plane and the MP-BGP Protocol as a Control plane.

The solution is based on the three applications located in [evpn-for-ovn/docker_files](https://github.com/romanspb80/evpn-for-ovn/tree/master/docker_files).
**evpn-api.py** is API server (Flask).
**mpbgp-agent.py** is SDN-application with concept borrowed from RYU-application [**rest_vtep.py**](https://ryu.readthedocs.io/en/latest/app/rest_vtep.html)
**ovsdb-agent.py** - is application to setup OVSDB.
These applications communicate by Rabbitmq broker.

The **rest_vtep.py** implements VTEP (VXLAN Tunnel Endpoint) for EVPN VXLAN in accordance with RFC7432. Also it sets OpenFlow rules in the OVS bridge for provisioning the connectivity of clients.

**evpn-api.py** implements REST-API. It receives http-requests from CMS and then makes RPC-calls to **mpbgp-agent.py** via RabbitMQ-server using OSLO messaging. RabbitMQ-server (AMQP broker) is a part of Openstack infrastructure. Then **mpbgp-agent.py** setup BGP-session with BGP-neighbor and send RPC-requests to **ovsdb-agent**. The concept of AMQP and OSLO messaging is described in the OpenStack documentation:

- https://docs.openstack.org/nova/latest/reference/rpc.html

- https://docs.openstack.org/oslo.messaging/latest/index.html

There are two main functions of **mpbgp-agent**:
1. BGP Speaker for MP-BGP signaling (Control Plane).
2. Creator of instructions for configuring the Northbound-OVSDB and OVS-OVSDB.

**ovsdb-agent** receives the instructions from **mpbgp-agent** and implements them.

In case of receiving requests from **evpn-api.py** the **mpbgp-agent** sent appropriate requests to an External System by MP-BGP. The requests can be such as "Connect to Neighbor", "Create new network", "Sent new client mac:ip.addr", etc. In case of **evpn-agent** receives requests by MP-BGP from Neighbor (External System) it sends appropriate instructions to **ovsdb-agent** to setup OVN and OVS: VXLAN-Port creating on "br-int" OVS bridge and mapping it to OVN Logical Port, mac:ip.addr creating or deleting in OVN Logical Switch, sending mac:ip.addr updates of clients to the Neighbor (Ext. System) by MP-BGP.


## Enviroment installation

*git clone https://github.com/romanspb80/evpn-for-ovn.git*
*cd ./evpn-for-ovn/vagrant*
*vagrant up*

Three virtual machines will be run: **ovncluster**, **ryu**. In accordance with diagram DataCenter A is associated with the **ryu** and DataCenter B with **ovncluster**.
The IP addresses of virtual machines are represented in vagrant/Vagrantfile:

192.168.10.10 **ryu**

192.168.10.20 **ovncluster**

Also needs to have a public ssh key in the home directory on the your host: [{Dir.home}/.ssh/id_rsa.pub(https://github.com/romanspb80/evpn-for-ovn/blob/master/vagrant/Vagrantfile#L10)

And needs to add "192.168.10.20  evpn-api.domain-x.com" to /etc/hosts where will be done requests.

##Usage Example
This example supposes the following environment:
```
Host **ryu** (192.168.10.10)             Host **ovncluster** (192.168.10.20)
+--------------------+                   +--------------------+
|  rest_vtep (RYU)   | --- BGP(EVPN) --- |     mpbgp-agent    |
+--------------------+                   +--------------------+
          |                                         |
          |                                         |       
          |                              +--------------------+
          |                              |     ovsdb-agent    |
          |                              +--------------------+        
          |                                         |
          |                                         |
+--------------------+                   +--------------------+
|     s1 (OVS)       | ===== vxlan ===== |     br-int (OVS)   |
+--------------------+                   +--------------------+
      (s1-eth1)                                   (vm1)   
          |                                         |
    192.168.222.11                            192.168.222.22       
      +--------+                                +--------+  
      |  s1h1  |                                |  vm1   |  
      +--------+                                +--------+
```

*ssh vagrant@192.168.10.10* makes connection to **ryu** host
*ssh vagrant@192.168.10.20* - to **ovncluster**

**Pre-setup**
Needs to create virtual hosts in the DataCenter A and DataCenter B
Vagrant run the script [evpn-for-ovn/scripts/pre-deploy.py](https://github.com/romanspb80/evpn-for-ovn/blob/master/scripts/pre-deploy.py)
and installed the Logical Switch ls1 with port ls1-vm1 and Network Namespace vm1 with port vm1 for **ovncluster**.
This script emulates the creation virtual machine in Openstack (or pod in Kubernetes with OVN-based CNI)
It can be check with commands on the host **ovncluster**:
sudo ip netns exec vm1 ip a
sudo ovn-nbctl show
sudo ovn-sbctl show

On **ryu** needs to create a virtual host manually with mininet:

*sudo mn --topo single,1 --mac --switch ovsk --controller remote*

*mininet> py h1.intf('h1-eth0').setMAC('02:ac:10:ff:00:11')*

*mininet> py h1.intf('h1-eth0').setIP('192.168.222.11/24')*

In another terminal run the **rest_vtep**:
*sudo ryu-manager --verbose --ofp-tcp-listen-port 6653 /usr/lib/python3/dist-packages/ryu/app/rest_vtep.py*


**Configuration steps**

1. Create a new BGPSpeaker instance on each host

For **ryu**:

$ curl -X POST -d '{"dpid": 1, "as_number": 65000, "router_id": "192.168.10.10"}' http://192.168.10.10:8080/vtep/speakers | python3 -m json.tool
```
{
    "192.168.10.10": {
        "EvpnSpeaker": {
            "as_number": 65000,
            "dpid": 1,
            "neighbors": {},
            "router_id": "192.168.10.10"
        }
    }
}
```
For **ovncluster**:

$ curl -X POST -H "Content-Type: application/json" -d '{"as_number": 65000, "router_id": "192.168.10.20"}' http://evpn-api.domain-x.com/vtep/speakers | python3 -m json.tool
```
{
    "192.168.10.20": {
        "EvpnSpeaker": {
            "as_number": 65000,
            "neighbors": {},
            "router_id": "192.168.10.20"
        }
    }
}
```
2. Request the neighbors on each hosts

For **ryu**:

$ curl -X POST -d '{"address": "192.168.10.20", "remote_as": 65000}' http://192.168.10.10:8080/vtep/neighbors | python3 -m json.tool
```
{
    "192.168.10.20": {
        "EvpnNeighbor": {
            "address": "192.168.10.20",
            "remote_as": 65000,
            "state": "down"
        }
    }
}
```
For **ovncluster**:

$ curl -X POST -H "Content-Type: application/json" -d '{"address": "192.168.10.10", "remote_as": 65000}' http://evpn-api.domain-x.com/vtep/neighbors | python3 -m json.tool
```
{
    "192.168.10.10": {
        "EvpnNeighbor": {
            "address": "192.168.10.10",
            "remote_as": 65000,
            "state": "down"
        }
    }
}
```
3. Defines a new VXLAN network(VNI=10)

For **ryu**:

$ curl -X POST -d '{"vni": 10}' http://192.168.10.10:8080/vtep/networks | python3 -m json.tool
```
{
    "10": {
        "EvpnNetwork": {
            "clients": {},
            "ethernet_tag_id": 0,
            "route_dist": "65000:10",
            "vni": 10
        }
    }
}
```
For **ovncluster**:

$ curl -X POST -H "Content-Type: application/json" -d '{"vni": 10, "logical_switch": "ls1"}' http://evpn-api.domain-x.com/vtep/networks |python3 -m json.tool
```
{
    "10": {
        "EvpnNetwork": {
            "clients": {},
            "ethernet_tag_id": 0,
            "logical_switch": "ls1",
            "route_dist": "65000:10",
            "vni": 10
        }
    }
}
```
4. Transmit the clients for associated VXLAN network

For **ryu**:

$ curl -X POST -d '{"port": "s1-eth1", "mac": "02:ac:10:ff:00:11", "ip": "192.168.222.11"} ' http://192.168.10.10:8080/vtep/networks/10/clients | python3 -m json.tool
```
{
    "10": {
        "EvpnClient": {
            "ip": "192.168.222.11",
            "mac": "02:ac:10:ff:00:11",
            "next_hop": "192.168.10.10",
            "port": 1
        }
    }
}
```
For **ovncluster**:

$ curl -X POST -H "Content-Type: application/json" -d '{"port": "vm1", "mac": "02:ac:10:ff:00:12", "ip": "192.168.222.12"} ' http://evpn-api.domain-x.com/vtep/networks/10/clients | python3 -m json.tool
```
{
    "10": {
        "EvpnClient": {
            "ip": "192.168.222.12",
            "mac": "02:ac:10:ff:00:12",
            "next_hop": "192.168.10.20",
            "port": "vm1"
        }
    }
}
```

5. Testing

In the console with mininet:

mininet> h1 ping 192.168.222.12
```
PING 192.168.222.12 (192.168.222.12) 56(84) bytes of data.
64 bytes from 192.168.222.12: icmp_seq=1 ttl=64 time=3.53 ms
64 bytes from 192.168.222.12: icmp_seq=2 ttl=64 time=0.856 ms
64 bytes from 192.168.222.12: icmp_seq=3 ttl=64 time=0.766 ms
64 bytes from 192.168.222.12: icmp_seq=4 ttl=64 time=1.19 ms
```

And ping from the virtual host on Datacenter B:
```
sudo  ip netns exec vm1 ping 192.168.222.11
```


# Sequence Diagram
![evpn4ovn](https://github.com/romanspb80/evpn-for-ovn/assets/30826451/18f79189-283f-4a71-b7e9-e39680fa561c)



# Further development
1. Implement more other runtime environments with MP-BGP such as JunOS, GoBGP, Quagga, etc...
2. Implement monitoring subsystem.
3. Develop evpn4ovn kubernetes operator.
