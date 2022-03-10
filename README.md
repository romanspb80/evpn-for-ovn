# evpn-for-ovn
A prototype of EVPN-VXLAN implementation for network cloud solution based on OVN.
# Introduction
OVN is described in the [document](https://www.ovn.org/support/dist-docs/ovn-architecture.7.txt):

"OVN, the Open Virtual Network, is a system to support virtual network abstraction. OVN complements the existing capabilities of OVS to add native support for virtual network abstractions, such as virtual L2 and L3 overlays and security groups. Services such as DHCP are also  desirable  features. Just like OVS, OVN’s design goal is to have a production-quality implementation that can operate at significant scale."

And the main purpose of OVN is a Control Plane constructing for Neutron OpenStack. Also there some solutions for Kubernetes networking model implementation: Kube-OVN, OVN4NFV-K8s-Plugin and OVN-Kubernetes.
OVN is built on the same architectural principles as VMware's commercial NSX and offers the same core network virtualization capability — providing a free alternative that has been adopted in open source orchestration systems. But OVN can not implement EVPN Multi-Site architecture for DCI (Data Center Interconnect) concept.

This project demonstrates how it can be solved with adding new applications of RYU. RYU is a SDN framework with the libraries of different network protocols and written in Python: https://ryu.readthedocs.io/en/latest/getting_started.html#what-s-ryu

# How it works
Assume that there are two data centers - DataCenter X and DataCenter Y. And needs to organize a EVPN connection between them. DataCenter X is an External System with EVPN-VXLAN support. In the project the built-in RYU-application **rest_vtep.py** is used as an External System. DataCenter Y is our Cloud Platform based on OpenStack with OVN.
The diagram shows the following elements.
1. DataCenter X (an External System) with the RYU-application rest_vtep.py
2. DataCenter Y includes:
- Microstack with OVN.
- K8S Cluster with our application for OVN EVPN-VXLAN implementation.
- Some kind of a Orchestrator or [Cloud Management System](https://en.wikipedia.org/wiki/Cloud_management) (CMS).
![Uploading Scheme5.jpg…]()
## Idea description
Needs to organize a L2VPN (distributed L2 network) between sites (Data Centers) using EVPN-VXLAN . One of them is our Openstack Platform (Microstack with OVN) - DataCenter Y. The other is external system with EVPN-VXLAN support - DataCenter X. The key step is the implementation of EVPN-VXLAN on Neutron side. To do this, it is necessary to implement the building vxlan tunnels as a Data plane and the MP-BGP Protocol as a Control plane.

The applications **evpn-api.py** and **evpn-agent.py** are located in [evpn-for-ovn/docker_files](https://github.com/romanspb80/evpn-for-ovn/tree/master/docker_files).
**evpn-api.py** - is a Flask API-server.
**evpn-agent.py** is a RYU-application whose concept is borrowed from the built-in application [**rest_vtep.py**](https://ryu.readthedocs.io/en/latest/app/rest_vtep.html)

The **rest_vtep.py** implements VTEP (VXLAN Tunnel Endpoint) for EVPN VXLAN in accordance with RFC7432. Also it sets OpenFlow rules in the OVS bridge for provisioning the connectivity of clients.

**evpn-api.py** implements REST-API. It receives http-requests from CMS and then makes RPC-calls to **evpn-agent.py** via RabbitMQ-server using OSLO messaging. RabbitMQ-server (AMQP broker) is a part of Openstack infrastructure. Then **evpn-agent.py** receives RPC-calls and performs tasks. The concept of AMQP and OSLO messaging is described in the OpenStack documentation:

- https://docs.openstack.org/nova/latest/reference/rpc.html

- https://docs.openstack.org/oslo.messaging/latest/index.html

There are two main functions of **evpn-agent.py**:
1. BGP Speaker for MP-BGP signaling (Control Plane).
2. VXLAN-Port creating (Data Plane). 

In case of getting requests from **evpn-api.py** the **evpn-agent.py** sent appropriate requests to an External System by MP-BGP. The requests can be such as "Connect to Neighbor", "Create new network", "Sent new client mac:ip.addr", etc. In case of **evpn-agent.py** get requests by MP-BGP from Neighbor (External System) it makes appropriate activities on OVN and OVS sides: VXLAN-Port creating on "br-int" OVS bridge and mapping it to OVN Logical Port, mac:ip.addr creating or deleting in OVN Logical Switch, sending mac:ip.addr updates of clients to the Neighbor (Ext. System) by MP-BGP.


## Usage Example and the sequence of actions
The main VTEP configuration steps are described in RYU doc:

https://ryu.readthedocs.io/en/latest/app/rest_vtep.html

There are some differences in REST API for **evpn-api.py**. The REST API descriptions are in the docstring.

## Enviroment installation

Enable the nested virtualization on your host machine. For example it is described here: https://docs.fedoraproject.org/en-US/quick-docs/using-nested-virtualization-in-kvm/
Perform:
cd ./evpn-for-ovn/vagrant/
vagrant up

Three virtual machines will be run: **devstack**, **k8s** ,**ryu**. In accordance with diagram DataCenter X is associated with the **ryu**, DataCenter Y - with **devstack** and **k8s**.
The IP addresses of virtual machines are represented in vagrant/Vagrantfile:
192.168.10.10 **ryu**

192.168.10.20 **k8s**

192.168.10.200 **devstack**

Login/Password: vagrant/vagrant

Also it is necessary to add "192.168.10.20  evpn-api.domain-x.com" to /etc/hosts where from will be done requests. And also  it should be to set CPU mode on **devstack** virtual machine in "host-model" or "host-passthrough".

##Usage Example
This example supposes the following environment:
```
Host **ryu** (192.168.10.10)             Host **k8s** (192.168.10.20)
+--------------------+                   +--------------------+
|  Ryu (rest_vtep)   | --- BGP(EVPN) --- |  Ryu (**evpn-api**)|
+--------------------+                   +--------------------+
        |                                       |
        |                                       |
        |                                Host **devstack** (192.168.10.200)
+--------------------+                   +--------------------+
|   s1 (OVS)         | ===== vxlan ===== |   br-int (OVS)     |
+--------------------+                   +--------------------+
(s1-eth1)    (s1-eth2)                   (port-test)   
    |           |                            |
 10.0.0.11      |                        10.0.0.22       
+--------+  +--------+                   +--------+  
| s1h1   |  | s1h2   |                   | vm-test|  
+--------+  +--------+                   +--------+
```


**Pre configuration**

On **ryu**:

*$ sudo mn --controller=remote,ip=127.0.0.1 --topo=single,2 --switch=ovsk,protocols=OpenFlow13 --mac*

*mininet> py h1.intf('h1-eth0').setMAC('02:ac:10:ff:00:11')*

*mininet> py h1.intf('h1-eth0').setIP('10.0.0.11/24')*

On **devstack**:

*$ openstack port create --network private --mac 02:ac:10:ff:00:22 --fixed-ip subnet=private-subnet,ip-address=10.0.0.22 port-test*

*IMAGE=$(openstack image list -f value -c Name | grep cirros)*

*openstack server create --flavor cirros256 --image $IMAGE --port port-test vm-test*


**Configuration steps**


1. Creates a new BGPSpeaker instance on each host

On **ryu**:

curl -X POST -d '{"dpid": 1, "as_number": 65000, "router_id": "192.168.10.10"}' http://192.168.10.10:8080/vtep/speakers | python -m json.tool

On **k8s**:

curl -X POST -d '{"as_number": 65000, "router_id": "192.168.10.20"}' http://evpn-api.domain-x.com/vtep/speakers | python -m json.tool



2. Registers the neighbor for the speakers on each host

On **ryu**:

curl -X POST -d '{"address": "192.168.10.20", "remote_as": 65000}' http://192.168.10.10:8080/vtep/neighbors | python -m json.tool

On **k8s**:

curl -X POST -d '{"address": "192.168.10.10", "remote_as": 65000}' http://evpn-api.domain-x.com/vtep/neighbors | python -m json.tool



3. Defines a new VXLAN network(VNI=10)

On **ryu**:

curl -X POST -d '{"vni": 10}' http://192.168.10.10:8080/vtep/networks |python -m json.tool

On **k8s**:

curl -X POST -d '{"vni": 10, "network_id": "7d29da33-5d12-4c04-95de-1672709ae946"}' http://evpn-api.domain-x.com/vtep/networks |python -m json.tool

Where param "network_id" is a Neutron Network Identifier. It is associated with Logical Switch in OVN.
Commands (requests) for list getting:

*$ openstack network list

*$ ovn-nbctl ls-list



4. Registers the clients to the VXLAN network.

On **ryu**:

curl -X POST -d '{"port": "s1-eth1", "mac": "02:ac:10:ff:00:11", "ip": "10.0.10.11"} ' http://192.168.10.10:8080/vtep/networks/10/clients | python -m json.tool

On **k8s**:

curl -X POST -d '{"port": "8f93d2ba-527a-44ea-9b4f-3ce2c6067588", "mac": "02:ac:10:ff:00:22", "ip": "10.0.0.22"} ' http://evpn-api.domain-x.com/vtep/networks/10/clients | python -m json.tool

Where param port (for **k8s**) is OVN Logical Port. It corresponds with Port ID Neutron:
```
$ ovn-nbctl show

switch ae3ef4dc-5c15-4964-b282-77d1ec430cd3 (neutron-7d29da33-5d12-4c04-95de-1672709ae946) (aka private)

......................................................................................

    port 8f93d2ba-527a-44ea-9b4f-3ce2c6067588 (aka port-test)
        addresses: ["02:ac:10:ff:00:22 10.0.0.22 fd97:23a0:78dc:0:ac:10ff:feff:22"]
......................................................................................
```

*$ openstack port show port-test -f value -c id*

***8f93d2ba-527a-44ea-9b4f-3ce2c6067588***



5. Testing

(s1h1)mininet> ping 10.0.0.22

(vm-test)$ ping 10.0.0.11


# Further development
1. Split the **evpn-agent.py** into two applications: "BGP Speaker" and "OVS/OVN Configurator". These apps will be connected via RabbitMQ. "OVS/OVN Configurator" would be get requests from different sources via Queue.
2. Orchestrate the app "BGP Speaker" with NodePort-type Service for associate containers with host-interfaces. This would get the opportunity to run several "BGP Speaker" on one host.
3. Implement monitoring subsystem.
4. Develop a plugin for OpenStack instead of current solution. It would be better to extend Neutron API and use only one Endpoint.
5. Develop WiKi.
