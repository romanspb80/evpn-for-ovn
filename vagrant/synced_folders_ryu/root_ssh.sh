#!/bin/bash

sudo su
apt-get install -y ssh
sed -i 's/PermitRootLogin prohibit-password/PermitRootLogin yes/g' /etc/ssh/sshd_config
echo "root:ryu" | chpasswd
service ssh restart