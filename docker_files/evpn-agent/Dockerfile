FROM osrg/ryu:latest
WORKDIR /root/ryu/ryu/app
COPY evpn-agent.py .
RUN apt-get update && apt-get install -y ssh python-dev build-essential libssl-dev libffi-dev libxml2-dev libxslt1-dev \
    zlib1g-dev python-pip
RUN pip install wheel
RUN pip install oslo.messaging
RUN pip install eventlet
RUN pip install paramiko
RUN sed -i 's/PermitRootLogin prohibit-password/PermitRootLogin yes/g' /etc/ssh/sshd_config
RUN echo "root:ryu" | chpasswd
ENTRYPOINT ["python", "/root/ryu/bin/ryu-manager"]
CMD ["--verbose", "/root/ryu/ryu/app/evpn-agent.py"]
