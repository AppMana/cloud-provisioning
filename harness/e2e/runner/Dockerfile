# The runner isolates host bridges, routes and the Docker inventory. Cluster
# nodes remain the QEMU guests selected by lab -rig vm.
FROM docker:29.7.2-dind@sha256:3ef33f2e220b79ed3ef3b99d81746f06f306cd6340e2cb7331d17ae996e74cb6
RUN apk add --no-cache aws-cli bash curl file git go iproute2 iptables kmod openssh-client python3 sudo tar util-linux \
    && apk info -vv > /runner-packages.txt \
    && git config --global --add safe.directory /workspace
COPY containerlab /usr/local/bin/containerlab
COPY helm /usr/local/bin/helm
ARG CONTAINERLAB_SHA256
ARG HELM_SHA256
RUN test -n "$CONTAINERLAB_SHA256" \
    && test -n "$HELM_SHA256" \
    && echo "$CONTAINERLAB_SHA256  /usr/local/bin/containerlab" | sha256sum -c - \
    && echo "$HELM_SHA256  /usr/local/bin/helm" | sha256sum -c - \
    && chmod 0755 /usr/local/bin/containerlab /usr/local/bin/helm \
    && file /usr/local/bin/helm | grep -Eq 'statically linked|static-pie linked' \
    && /usr/local/bin/containerlab version \
    && /usr/local/bin/helm version --short
ENV CGO_ENABLED=0
# A private Unix socket avoids exposing a second unauthenticated Docker API.
ENTRYPOINT ["dockerd-entrypoint.sh", "dockerd"]
CMD ["--host=unix:///var/run/docker.sock", "--storage-driver=overlay2"]
