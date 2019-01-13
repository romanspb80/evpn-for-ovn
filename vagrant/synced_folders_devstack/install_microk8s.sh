#!/bin/bash

sudo systemctl disable firewalld && sudo systemctl stop firewalld
sudo apt install -y docker.io
sudo snap install microk8s --classic
cd ./k8s
microk8s.kubectl create -f evpn-api-ing.yaml
microk8s.kubectl create -f evpn-api-rs.yaml
microk8s.kubectl create -f evpn-api-svc.yaml
microk8s.kubectl create -f evpn-agent-rs.yaml
microk8s.kubectl create -f evpn-agent-svc.yaml
microk8s.kubectl create configmap app-settings --from-file=settings