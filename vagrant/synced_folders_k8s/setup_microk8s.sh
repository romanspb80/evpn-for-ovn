#!/bin/bash

microk8s enable dns ingress helm3
snap alias microk8s.kubectl kubectl
snap alias microk8s.helm3 helm3
