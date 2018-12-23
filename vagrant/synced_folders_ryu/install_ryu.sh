#!/bin/bash

sudo apt-get install python-dev python-setuptools python-pip
git clone https://github.com/osrg/ryu.git
cd ryu
pip install .
cd ..
git clone https://github.com/romanspb80/evpn-for-ovn.git
cp evpn-for-ovn/rest_vtep_* ryu/ryu/app/
