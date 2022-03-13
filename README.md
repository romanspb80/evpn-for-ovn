# evpn-for-ovn
A prototype of EVPN-VXLAN implementation for network cloud solution based on OVN.
# Introduction
OVN is described in the [document](https://www.ovn.org/support/dist-docs/ovn-architecture.7.txt):

"OVN, the Open Virtual Network, is a system to support virtual network abstraction. OVN complements the existing capabilities of OVS to add native support for virtual network abstractions, such as virtual L2 and L3 overlays and security groups. Services such as DHCP are also  desirable  features. Just like OVS, OVN’s design goal is to have a production-quality implementation that can operate at significant scale."

And the main purpose of OVN is a Control Plane constructing for Neutron OpenStack. Also there some solutions for Kubernetes networking model implementation: Kube-OVN, OVN4NFV-K8s-Plugin and OVN-Kubernetes.
OVN is built on the same architectural principles as VMware's commercial NSX and offers the same core network virtualization capability — providing a free alternative that has been adopted in open source orchestration systems. But OVN can not implement EVPN Multi-Site architecture for DCI (Data Center Interconnect) concept.

This project demonstrates how it can be solved with adding new applications of RYU. RYU is a SDN framework with the libraries of different network protocols and written in Python: https://ryu.readthedocs.io/en/latest/getting_started.html#what-s-ryu

# How it works
Assume that there are two data centers - DataCenter A and DataCenter B. And needs to organize a EVPN connection between them. DataCenter A is an External System with EVPN-VXLAN support. In the project the built-in RYU-application **rest_vtep.py** is used as an External System. DataCenter B is our Cloud Platform based on OpenStack with OVN.
The diagram shows the following elements.
1. DataCenter A (an External System) with the RYU-application rest_vtep.py
2. DataCenter B includes:
- Microstack with OVN.
- K8S Cluster (Microk8s) with our applications for OVN EVPN-VXLAN implementation.
- Some kind of an Orchestrator or [Cloud Management System](https://en.wikipedia.org/wiki/Cloud_management) (CMS).
![Scheme5](https://user-images.githubusercontent.com/30826451/157813554-fa0464ea-9189-43ed-93be-9656f06c1e1d.jpg)
## Idea description
Needs to organize a L2VPN (distributed L2 network) between sites (Data Centers) using EVPN-VXLAN . One of them is our Openstack Platform (Microstack with OVN) - DataCenter B. Another is external system with EVPN-VXLAN support - DataCenter A. The key step is the implementation of EVPN-VXLAN on Neutron side. To do this, it is necessary to implement the building vxlan tunnels as a Data plane and the MP-BGP Protocol as a Control plane.

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

*git clone https://github.com/romanspb80/evpn-for-ovn.git*
*cd ./evpn-for-ovn/vagrant*
*vagrant up*

Three virtual machines will be run: **microstack**, **ryu**. In accordance with diagram DataCenter A is associated with the **microstack** and DataCenter B with **ryu**.
The IP addresses of virtual machines are represented in vagrant/Vagrantfile:

192.168.10.10 **ryu**

192.168.10.20 **microstack**

Also needs to have a public ssh key in the home directory on the your host: [{Dir.home}/.ssh/id_rsa.pub(https://github.com/romanspb80/evpn-for-ovn/blob/master/vagrant/Vagrantfile#L10)

Also it is necessary to add "192.168.10.20  evpn-api.domain-x.com" to /etc/hosts where will be done requests.

##Usage Example
This example supposes the following environment:
```
Host **ryu** (192.168.10.10)             Host **microstack** (192.168.10.20)
+--------------------+                   +--------------------+
|  rest_vtep (RYU)   | --- BGP(EVPN) --- |   evpn-agent (RYU) |
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


**Pre-setup**

Needs to create virtual hosts in the DataCenter A and DataCenter B
Connect to the **ryu** and **microstack**:

*ssh vagrant@192.168.10.10*

*ssh vagrant@192.168.10.20*

In **ryu** create a virtual host with mininet:

*sudo mn --topo single,1 --mac --switch ovsk --controller remote*

*mininet> py h1.intf('h1-eth0').setMAC('02:ac:10:ff:00:11')*

*mininet> py h1.intf('h1-eth0').setIP('192.168.222.11/24')*

In another terminal run the **rest_vtep**:

*cd /usr/local/bin ; sudo ./ryu-manager --verbose --ofp-tcp-listen-port 6653 ../lib/python3.8/dist-packages/ryu/app/rest_vtep.py*


In **microstack** create a virtual machine:

*ssh-keygen -q -N "" -f ~/.ssh/id_rsa*

*openstack keypair create --public-key ~/.ssh/id_rsa.pub mykey*

*openstack flavor create --public m1.extra_tiny --id auto --ram 256 --disk 0 --vcpus 1*

*openstack port create --network test --mac 02:ac:10:ff:00:22 --fixed-ip subnet=test-subnet,ip-address=192.168.222.22 port-test*

*openstack floating ip create external --port port-test*

*openstack server create --flavor m1.extra_tiny --image cirros --port port-test --key-name mykey vm-test*


*NETWORK_ID=$(openstack network list --name test -f value -c ID)*

*PORT_ID=$(openstack port show port-test -f value -c id)*

*SERVER_IP=$(openstack floating ip list --port port-test -f value | awk  '{print $2}')*



**Configuration steps**


1. Create a new BGPSpeaker instance on each host

For **ryu**:

curl -X POST -d '{"dpid": 1, "as_number": 65000, "router_id": "192.168.10.10"}' http://192.168.10.10:8080/vtep/speakers | python3 -m json.tool

For **microstack**:

curl -X POST -H "Content-Type: application/json" -d '{"as_number": 65000, "router_id": "192.168.10.20"}' http://evpn-api.domain-x.com/vtep/speakers | python3 -m json.tool

2. Request the neighbors on each hosts

For **ryu**:

curl -X POST -d '{"address": "192.168.10.20", "remote_as": 65000}' http://192.168.10.10:8080/vtep/neighbors | python3 -m json.tool

For **microstack**:

curl -X POST -H "Content-Type: application/json" -d '{"address": "192.168.10.10", "remote_as": 65000}' http://evpn-api.domain-x.com/vtep/neighbors | python3 -m json.tool

3. Defines a new VXLAN network(VNI=10)

For **ryu**:

curl -X POST -d '{"vni": 10}' http://192.168.10.10:8080/vtep/networks | python3 -m json.tool

For **microstack**:

*curl -X POST -H "Content-Type: application/json" -d '{"vni": 10, "network_id": "<the value of $NETWORK_ID>"}' http://evpn-api.domain-x.com/vtep/networks |python3 -m json.tool*

Where param "network_id" is a Neutron Network Identifier. It is associated with Logical Switch in OVN.


4. Transmit the clients for associated VXLAN network

For **ryu**:

*curl -X POST -d '{"port": "s1-eth1", "mac": "02:ac:10:ff:00:11", "ip": "192.168.222.11"} ' http://192.168.10.10:8080/vtep/networks/10/clients | python3 -m json.tool*

For **microstack**:

*curl -X POST -H "Content-Type: application/json" -d '{"port": "<the value of $PORT_ID>", "mac": "02:ac:10:ff:00:22", "ip": "192.168.222.22"} ' http://evpn-api.domain-x.com/vtep/networks/10/clients | python3 -m json.tool*

Where param "port" is OVN Logical Port.

5. Needs to change vxlan udp-port of vxlan in the **ryu** due to the fact that it is busy by another service on the **microstack** side:

*sudo ovs-vsctl set interface vxlan_192.168.10.20_10 options:dst_port=4788*

5. Testing

In the console with mininet:

`mininet> h1 ping -c 3 192.168.222.22
PING 192.168.222.22 (192.168.222.22) 56(84) bytes of data.
64 bytes from 192.168.222.22: icmp_seq=1 ttl=64 time=10.4 ms
64 bytes from 192.168.222.22: icmp_seq=2 ttl=64 time=2.25 ms
64 bytes from 192.168.222.22: icmp_seq=3 ttl=64 time=1.50 ms

--- 192.168.222.22 ping statistics ---
3 packets transmitted, 3 received, 0% packet loss, time 2004ms
rtt min/avg/max/mdev = 1.503/4.708/10.377/4.019 ms`


In the **microstack** connect to the virtual machine:

*ssh-keyscan $SERVER_IP >> ~/.ssh/known_hosts*

*sshpass -p gocubsgo ssh -i ~/.ssh/id_rsa.pub cirros@$SERVER_IP*

And ping the remote host:

`$ ping -c 3 192.168.222.11
PING 192.168.222.11 (192.168.222.11): 56 data bytes
64 bytes from 192.168.222.11: seq=0 ttl=64 time=4.975 ms
64 bytes from 192.168.222.11: seq=1 ttl=64 time=2.036 ms
64 bytes from 192.168.222.11: seq=2 ttl=64 time=1.266 ms

--- 192.168.222.11 ping statistics ---
3 packets transmitted, 3 packets received, 0% packet loss
round-trip min/avg/max = 1.266/2.759/4.975 ms`

# Sequence Diagram
![sequenceDiagram](https://user-images.githubusercontent.com/30826451/158036290-d9078788-7ce5-438f-98be-73b00bae86b7.png)

# Further development
1. Split the **evpn-agent.py** into two applications: "BGP Speaker" and "OVS/OVN Configurator". These apps will be connected via RabbitMQ. "OVS/OVN Configurator" would be get requests from different sources via Queue.
2. Orchestrate the app "BGP Speaker" with NodePort-type Service for associate containers with host-interfaces. This would get the opportunity to run several "BGP Speaker" on one host.
3. Implement monitoring subsystem.
4. Develop a plugin for OpenStack instead of current solution. It would be better to extend Neutron API and use only one Endpoint.
5. Develop WiKi.
