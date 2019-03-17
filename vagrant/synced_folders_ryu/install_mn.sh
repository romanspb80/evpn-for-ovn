#!/bin/bash

sudo apt-get update
sudo apt-get install -y git
sudo apt-get install -y mininet
sudo ovs-vsctl set-manager ptcp:6640