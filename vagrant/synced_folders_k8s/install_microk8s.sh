#!/bin/bash

sudo apt-get install -y snapd
sudo snap install microk8s --classic
sudo snap disable microk8s
sudo snap enable microk8s
sudo sleep 1m
cd ./k8s
sudo microk8s.enable dns
sudo microk8s.kubectl create configmap app-settings --from-file=settings/app_settings.py
sudo microk8s.kubectl create -f evpn-api-ing.yaml
sudo microk8s.kubectl create -f evpn-api-rs.yaml
sudo microk8s.kubectl create -f evpn-api-svc.yaml
sudo microk8s.kubectl create -f evpn-agent-rs.yaml
sudo microk8s.kubectl create -f evpn-agent-svc.yaml