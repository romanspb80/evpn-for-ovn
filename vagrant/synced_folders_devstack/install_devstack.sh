#!/bin/bash

sudo apt-get update
sudo apt-get install -y git
sudo apt-get install -y crudini

git clone https://git.openstack.org/openstack-dev/devstack.git
./devstack/tools/create-stack-user.sh
sudo su -l stack -c "git clone https://git.openstack.org/openstack-dev/devstack.git"
sudo su -l stack -c "git clone https://git.openstack.org/openstack/networking-ovn.git"
#sudo su -l stack -c "cd devstack && git checkout 698796f1aeb0d9a559488bad9f1d03e4941b061e && cd .."
sudo su -l stack -c "cp ./networking-ovn/devstack/local.conf.sample ./devstack/local.conf"
sudo su -l stack -c "./devstack/stack.sh"
