#!/bin/bash

sudo sed -i 's/PermitRootLogin prohibit-password/PermitRootLogin yes/g' /etc/ssh/sshd_config
sudo echo "root:password" | chpasswd
sudo service ssh restart
