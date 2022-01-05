#!/bin/bash

microk8s.enable dns ingress
microk8s.kubectl create configmap app-settings --from-file=settings/app_settings.py
microk8s.kubectl create -f evpn-api-ing.yaml
microk8s.kubectl create -f evpn-api-rs.yaml
microk8s.kubectl create -f evpn-api-svc.yaml
microk8s.kubectl create -f evpn-agent-svc.yaml
microk8s.kubectl create -f evpn-agent-ds.yaml
microk8s.kubectl create -f rabbitmq-svc.yaml
microk8s.kubectl create -f rabbitmq-endpnt.yaml
microk8s.kubectl create -f bgp-svc.yaml
microk8s.kubectl create -f bgp-endpnt.yaml
microk8s.kubectl create -f ovsdb-svc.yaml
microk8s.kubectl create -f ovsdb-endpnt.yaml
