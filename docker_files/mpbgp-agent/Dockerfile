FROM python:3.8

WORKDIR /app

COPY . .
RUN pip3 install -r requirements.txt

ENTRYPOINT ["ryu-manager"]
CMD ["--verbose", "mpbgp-agent.py"]
