#!/bin/bash

su -l stack -c "git clone https://git.openstack.org/openstack-dev/devstack.git"
su -l stack -c "git clone https://git.openstack.org/openstack/networking-ovn.git"
#su -l stack -c "cd devstack && git checkout 698796f1aeb0d9a559488bad9f1d03e4941b061e && cd .."
su -l stack -c "cp ./networking-ovn/devstack/local.conf.sample ./devstack/local.conf"
su -l stack -c "./devstack/stack.sh"
