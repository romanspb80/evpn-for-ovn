#!/bin/bash

sudo systemctl disable firewalld && sudo systemctl stop firewalld
sudo apt install -y docker.io
sudo apt install snapd
sudo snap install microk8s --classic
cd ./k8s
sudo microk8s.kubectl create -f evpn-api-ing.yaml
sudo microk8s.kubectl create -f evpn-api-rs.yaml
sudo microk8s.kubectl create -f evpn-api-svc.yaml
sudo microk8s.kubectl create -f evpn-agent-rs.yaml
sudo microk8s.kubectl create -f evpn-agent-svc.yaml
sudo microk8s.kubectl create configmap app-settings --from-file=settings