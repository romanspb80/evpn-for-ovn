#!/bin/bash
sudo su
sed -i 's/PermitRootLogin prohibit-password/PermitRootLogin yes/g' /etc/ssh/sshd_config
echo "root:password" | chpasswd
service ssh restart
