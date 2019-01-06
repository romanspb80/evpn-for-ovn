FROM osrg/ryu:latest
WORKDIR /root/ryu/ryu/app
COPY rest_vtep_client.py .
RUN pip install oslo.messaging 
ENTRYPOINT ["python", "/root/ryu/bin/ryu-manager"]
CMD ["--verbose", "/root/ryu/ryu/app/rest_vtep_client.py"]

