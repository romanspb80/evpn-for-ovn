FROM osrg/ryu:latest
WORKDIR /root/ryu/ryu/app
COPY rest_vtep_client.py .
COPY rest_vtep_client.sh .
RUN pip install oslo.messaging 
ENTRYPOINT /root/ryu/ryu/app/rest_vtep_client.sh

