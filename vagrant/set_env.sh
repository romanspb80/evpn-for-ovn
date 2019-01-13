#!/bin/sh

cp
#unset $(grep -v '^#' ./synced_folders_devstack/k8s/settings/app_settings.py | sed -E 's/(.*)=.*/\1/' | xargs)
#export $(grep -v '^#' ./synced_folders_devstack/k8s/settings/app_settings.py | xargs -d '\n')