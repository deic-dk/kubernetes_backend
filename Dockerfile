FROM ubuntu:22.04

RUN DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get -y install \
    ca-certificates

RUN mkdir /tmp/podcaches

ADD ["main", "/opt/user_pods_backend/main"]

ADD ["config.yaml", "/opt/user_pods_backend/config.yaml"]

WORKDIR "/opt/user_pods_backend"

CMD ["/opt/user_pods_backend/main"]
