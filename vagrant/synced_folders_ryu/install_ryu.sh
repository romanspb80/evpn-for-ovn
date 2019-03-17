#!/bin/bash

sudo su
apt-get install -y python-dev python-setuptools python-pip iptables
pip install paramiko
cd ~
git clone https://github.com/osrg/ryu.git
cd ryu
pip install .
#sudo su && iptables -t nat -A PREROUTING -p TCP --dport 179 -j REDIRECT --to-port 30179