FROM fedora:43@sha256:a651ddf48ea28a06ed4e1e6519f51c9f47e7a5a138722ade87369b8fbb7e5b42
RUN dnf install -y nmstate-2.2.57-2.fc43 coreos-installer-0.26.0-2.fc43 dnsmasq ca-certificates && dnf clean all
ENV PATH=/tools:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin
ENTRYPOINT ["/tools/openshift-install"]
