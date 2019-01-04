#!/bin/bash

sudo systemctl disable firewalld && sudo systemctl stop firewalld
sudo apt install -y docker.io
sudo snap install microk8s --classic

