FROM osrg/ryu:latest
WORKDIR /root/ryu/ryu/app
COPY evpn-api.py .
RUN echo "root:ryu" | chpasswd
RUN pip install oslo.messaging
RUN apt-get update && apt-get install -y ssh
RUN sed -i 's/PermitRootLogin prohibit-password/PermitRootLogin yes/g' /etc/ssh/sshd_config
RUN service ssh start
ENTRYPOINT ["python", "/root/ryu/bin/ryu-manager"]
CMD ["--verbose", "/root/ryu/ryu/app/evpn-api.py"]
