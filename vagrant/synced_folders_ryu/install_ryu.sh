#!/bin/bash

apt-get update
apt-get install -y git

sudo apt-get install mininet

sudo apt-get install git python-dev python-setuptools python-pip
git clone https://github.com/osrg/ryu.git
cd ryu
pip install .
