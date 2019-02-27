FROM osrg/ryu:latest
WORKDIR /root/ryu/ryu/app
COPY evpn-api.py .
RUN apt-get update && apt-get install -y ssh python3-dev python3-pip
RUN pip3 install oslo.messaging
RUN sed -i 's/PermitRootLogin prohibit-password/PermitRootLogin yes/g' /etc/ssh/sshd_config
RUN service ssh start
RUN echo "root:ryu" | chpasswd
ENTRYPOINT ["python", "/root/ryu/bin/ryu-manager"]
CMD ["--verbose", "/root/ryu/ryu/app/evpn-api.py"]
