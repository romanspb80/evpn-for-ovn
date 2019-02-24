# evpn-for-ovn
A prototype of EVPN-VXLAN implementation for network cloud solution based on OVN.
# Introduction
OVN is described in the document http://www.openvswitch.org/support/dist-docs/ovn-architecture.7.txt:

"OVN, the Open Virtual Network, is a system to support virtual network abstraction. OVN complements the existing capabilities of OVS to add native support for virtual network abstractions, such as virtual L2 and L3 overlays and security groups. Services such as DHCP are also  desirable  features. Just like OVS, OVN’s design goal is to have a production-quality implementation that can operate at significant scale."

And the main purpose of OVN is a Control Plane constructing for Neutron OpenStack. OVN is built on the same architectural principles as VMware's commercial NSX and offers the same core network virtualization capability — providing a free alternative that has been adopted in open source orchestration systems. But OVN can not implement EVPN Multi-Site architecture for DCI (Data Center Interconnect) concept.

This project demonstrates how it can be decided with adding new applications of RYU. RYU is a SDN framework with the libraries of different network protocols and written in Python: https://ryu.readthedocs.io/en/latest/getting_started.html#what-s-ryu

# How it works
Assume that there are two DC - DataCenter X and DataCenter Y and you want to organize a EVPN connection between them. DataCenter X is an External System with EVPN-VXLAN support. In the project the standard built-in RYU-application **rest_vtep.py** is used as an External System. DataCenter Y is your Cloud System based on OpenStack with OVN.
The diagram shows the following elements.
1. DataCenter X (an External System) with the standard RYU-application rest_vtep.py
2. DataCenter Y with:
- DevStack with OVN.
- The K8S Cluster with new RYU-applications for OVN EVPN-VXLAN implementation.
- Some kind of a Cloud Manager Appliction (Orchestrator).

![scheme2](https://user-images.githubusercontent.com/30826451/52918497-c9ce3480-3308-11e9-8fd0-b0a153bf8b62.jpg)
## Idea description
There is the task to organize a L2VPN (distributed L2 network) between sites (Data Centers) using EVPN-VXLAN . One of them is our Openstack Platform (DevStack with OVN) - DataCenter Y. The other is external system (with EVPN-VXLAN support) - DataCenter X. The key step is the implementation of EVPN-VXLAN on Neutron side. To do this, it is necessary to extend our Cloud Platform with the functionality of building vxlan tunnels and the implementation of the MP-BGP Protocol.

The applications **evpn-api.py** and **evpn-agent.py** are located in evpn-for-ovn/docker_files/ and written from Built-in Ryu application rest_vtep.py:

https://ryu.readthedocs.io/en/latest/app/rest_vtep.html

This built-in Ryu application implement VTEP (VXLAN Tunnel Endpoint) for EVPN VXLAN in accordance with RFC7432. Also it sets OpenFlow rules in the OVS bridge for provisioning the connectivity of clients.

The main idea of using two applications instead of one is to divide functions REST-API and handling. Also **evpn-agent.py** sets OVN-controller instead of using OpenFlow directly.

**evpn-api.py** implements REST-API. It gets http requests from a Cloud Manager Appliction and then makes RPC-calls to **evpn-agent.py** via RabbitMQ-server using OSLO messaging (deployed by Devtack). Then **evpn-agent.py** gets RPC-calls and performs tasks. It imlement concept of RPC in the OpenStack:

https://docs.openstack.org/nova/latest/reference/rpc.html

https://docs.openstack.org/oslo.messaging/latest/index.html

There are two main functions of **evpn-agent.py**:
1. VXLAN-Port configuration for OVS and OVN. 
2. BGP Speaker (MP-BGP signaling).

In case of getting requests from **evpn-api.py** the **evpn-agent.py** sent appropriate requests to an External System by MP-BGP. The requests can be such as "Connect to Neighbor", "Create new network", "Sent new client mac:ip.addr", etc. In case of **evpn-agent.py** get requests by MP-BGP from Neighbor (External System) it makes appropriate activities on OVN and OVS sides: VXLAN-Port creating, mac:ip.addr creating or deleting in OVN Logical_Switch.


## Usage Example and the sequence of actions
The main VTEP configuration steps are described in RYU doc:

https://ryu.readthedocs.io/en/latest/app/rest_vtep.html

There are some differences in REST API for **evpn-api.py**. The REST API descriptions are in the docstring.

## Enviroment installation

cd ./evpn-for-ovn/vagrant/

vagrant up

Three virtual machines will be run: **devstack**, **k8s** ,**ryu**. In accordance with diagram DataCenter X is associated with the **ryu**, DataCenter Y - with **devstack** and **k8s**.
IP addresses of virtual machines are represented in vagrant/Vagrantfile.
Login/Password: vagrant/vagrant


# Further development
1. Separate function "BGP Speaker" from **evpn-agent.py** and develop new application.
2. Develop a plugin for OpenStack instead of **evpn-agent.py**.
3. Develop WiKi.
