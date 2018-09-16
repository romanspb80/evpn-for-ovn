# evpn-for-ovn
A prototype of EVPN-VXLAN implementation for network cloud solution based on OVN.
# Introduction
OVN is described in the document http://www.openvswitch.org/support/dist-docs/ovn-architecture.7.txt:

"OVN, the Open Virtual Network, is a system to support virtual network abstraction. OVN complements the existing capabilities of OVS to add native support for virtual network abstractions, such as virtual L2 and L3 overlays and security groups. Services such as DHCP are also  desirable  features. Just like OVS, OVN’s design goal is to have a production-quality implementation that can operate at significant scale."

And the main purpose of OVN is a Control Plane constructing for Neutron OpenStack. OVN is built on the same architectural principles as VMware's commercial NSX and offers the same core network virtualization capability — providing a free alternative that has been adopted in open source orchestration systems. But OVN can not implement EVPN Multi-Site architecture for DCI (Data Center Interconnect) concept.

This project demonstrates how it can be decided with adding new applications of RYU. RYU is a SDN framework with the libraries of different network protocols and written in Python: https://ryu.readthedocs.io/en/latest/getting_started.html#what-s-ryu

# How it works
The diagram shows the following elements:
- DevStack with OVN.
- External system with EVPN-VXLAN support.
- The new RYU-applications for EVPN-VXLAN implementation.
- Some kind of a Cloud Manager Appliction (Management application for Openstack).

![scheme](https://user-images.githubusercontent.com/30826451/45481494-52dab180-b754-11e8-8060-8beb4625733c.jpg)
## Idea description
There is the task to organize a L2VPN (distributed L2 network) between sites (Data Centers) using EVPN-VXLAN . One of them is our Openstack Platform (DevStack with OVN). The other is external system (with EVPN-VXLAN support). The key step is the implementation of EVPN-VXLAN on Neutron side. To do this, it is necessary to extend our Cloud Platform with the functionality of building vxlan tunnels and the implementation of the MP-BGP Protocol.

Applications **rest_vtep_client.py** and **rest_vtep_server.py** are written from Built-in Ryu application rest_vtep.py:

https://ryu.readthedocs.io/en/latest/app/rest_vtep.html

This Built-in Ryu application implement VTEP (VXLAN Tunnel Endpoint) for EVPN VXLAN in accordance with RFC7432. Also it sets OpenFlow rules in the OVS bridge for provisioning the connectivity of clients.

The main idea of using two applications instead of one is to divide functions REST-API and handling. Also **rest_vtep_server.py** sets OVN-controller instead of using OpenFlow directly.

**rest_vtep_client.py** implements REST-API. It gets http requests from a Cloud Manager Appliction and then makes RPC-calls to **rest_vtep_server.py** via RabbitMQ-server using OSLO messaging. Then **rest_vtep_server.py** gets RPC-calls and performs tasks. It imlement concept of RPC in the OpenStack:

https://docs.openstack.org/nova/latest/reference/rpc.html

https://docs.openstack.org/oslo.messaging/latest/index.html

## Usage Example and the sequence of actions
The main VTEP configuration steps are described in RYU doc:
https://ryu.readthedocs.io/en/latest/app/rest_vtep.html
There are some differences in REST API for rest_vtep_client.py. The descriptions are in docstring.

## Enviroment installation
1. DevStack with OVN installation
https://docs.openstack.org/networking-ovn/latest/contributor/testing.html

2. RYU installation
https://ryu.readthedocs.io/en/latest/getting_started.html
