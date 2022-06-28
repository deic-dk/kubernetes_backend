FROM ubuntu:22.04

RUN DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get -y install \
    golang-1.18

RUN useradd userpods

ADD --chown=userpods:userpods ["src", "/opt/user_pods_backend"]

USER userpods

CMD ["sleep", "10000000000"]
