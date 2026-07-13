FROM ubuntu:24.04

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    systemd systemd-sysv wireguard-tools iproute2 iputils-ping bird2 iptables \
    && apt-get clean && rm -rf /var/lib/apt/lists/* \
    && rm -f /lib/systemd/system/multi-user.target.wants/* \
             /etc/systemd/system/*.wants/* \
             /lib/systemd/system/local-fs.target.wants/* \
             /lib/systemd/system/sockets.target.wants/*udev* \
             /lib/systemd/system/sockets.target.wants/*initctl* \
             /lib/systemd/system/basic.target.wants/* \
             /lib/systemd/system/anaconda.target.wants/*

COPY wg-pullup.sh /usr/local/sbin/wg-pullup
COPY wg-pullup@.service /etc/systemd/system/wg-pullup@.service
RUN chmod +x /usr/local/sbin/wg-pullup

# The real analogue of a cloud provider's user_data / cloud-init runcmd:
# a first-boot script that writes this instance's peer config and enables
# the unit. Baked into ExecStartPre of a oneshot so it runs once per boot,
# same as cloud-init.
COPY firstboot.sh /usr/local/sbin/firstboot
COPY firstboot.service /etc/systemd/system/firstboot.service
RUN chmod +x /usr/local/sbin/firstboot && \
    systemctl enable firstboot.service

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
