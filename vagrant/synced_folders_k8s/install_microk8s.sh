#!/bin/bash

kubectl create configmap app-settings --from-file=settings/app_settings.py
kubectl create -f evpn-api-ing.yaml
kubectl create -f evpn-api-rs.yaml
kubectl create -f evpn-api-svc.yaml
kubectl create -f evpn-agent-svc.yaml
kubectl create -f evpn-agent-ds.yaml
kubectl create -f rabbitmq-svc.yaml
kubectl create -f rabbitmq-endpnt.yaml
kubectl create -f bgp-svc.yaml
kubectl create -f bgp-endpnt.yaml
kubectl create -f ovsdb-svc.yaml
kubectl create -f ovsdb-endpnt.yaml
