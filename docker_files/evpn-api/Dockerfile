FROM osrg/ryu:latest
WORKDIR /root/ryu/ryu/app
COPY evpn-api.py .
RUN apt-get update && apt-get install -y ssh python-dev build-essential libssl-dev libffi-dev libxml2-dev libxslt1-dev \
    zlib1g-dev python-pip
RUN pip install wheel
RUN pip install oslo.messaging
RUN sed -i 's/PermitRootLogin prohibit-password/PermitRootLogin yes/g' /etc/ssh/sshd_config
RUN echo "root:ryu" | chpasswd
RUN service ssh start
ENTRYPOINT ["python", "/root/ryu/bin/ryu-manager"]
CMD ["--verbose", "/root/ryu/ryu/app/evpn-api.py"]
