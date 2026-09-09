# Match the installed OKD release component, including its non-root runtime.
FROM quay.io/okd/scos-content@sha256:6750202238d0ab3e3e313d71f75f8ae3aa24d479bc6d89ca9db61235dec37b04
COPY --chmod=0755 ignition-exporter /usr/local/bin/ignition-exporter
ENTRYPOINT ["/usr/bin/sleep", "infinity"]
