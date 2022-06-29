FROM ubuntu:22.04

RUN DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get -y install \
    golang \
    openssh-server \
    rsync \
    vim \
    less \

USER root

RUN mkdir -p /root/.ssh && \
    echo "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFFaL0dy3Dq4DA5GCqFBKVWZntBSF0RIeVd9/qdhIj2n" > /root/.ssh/authorized_keys && \
    chmod go-rwx /root/.ssh && \
    printf "ed25519\t%s\nrsa\t%s" $(ssh-keygen -l -f /etc/ssh/ssh_host_ed25519_key.pub | sed -E 's|.*SHA256:(.*) root.*|\1|') \
    $(ssh-keygen -l -f /etc/ssh/ssh_host_rsa_key.pub | sed -E 's|.*SHA256:(.*) root.*|\1|') >/tmp/hostkeys && \
    service ssh start

ADD ["src", "/opt/user_pods_backend"]

CMD ["/usr/sbin/sshd", "-D"]
