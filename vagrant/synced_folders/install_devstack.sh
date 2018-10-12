#!/bin/bash

git clone https://git.openstack.org/openstack-dev/devstack.git
./devstack/tools/create-stack-user.sh
su -l stack -c "git clone https://git.openstack.org/openstack-dev/devstack.git"
su -l stack -c "git clone https://git.openstack.org/openstack/networking-ovn.git"
su -l stack -c "cp ./networking-ovn/devstack/local.conf.sample ./devstack/local.conf"
su -l stack -c "./devstack/stack.sh"
