FROM osrg/ryu:latest
WORKDIR /root/ryu/ryu/app
COPY evpn-api.py .
RUN pip install oslo.messaging 
ENTRYPOINT ["python", "/root/ryu/bin/ryu-manager"]
CMD ["--verbose", "/root/ryu/ryu/app/evpn-api.py"]