FROM osrg/ryu:latest
COPY rest_vtep_client.py /root/ryu/ryu/app/rest_vtep_client.py
COPY ryu-ovn_client.py /root/ryu/ryu/app/ryu-ovn_client.py
RUN pip install oslo.messaging 
ENTRYPOINT ["python", "/root/ryu/ryu/app/ryu-ovn_client.py"]

