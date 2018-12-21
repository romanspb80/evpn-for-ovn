#!/bin/bash

sudo apt-get install python-dev python-setuptools python-pip
git clone https://github.com/osrg/ryu.git
cd ryu
pip install .
