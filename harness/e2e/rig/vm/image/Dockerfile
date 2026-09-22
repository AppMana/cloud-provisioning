ARG LABCONTAINERS_VM_IMAGE
FROM ${LABCONTAINERS_VM_IMAGE}
RUN test -x /labcontainers-guest && test -f /labcontainers-launch.py && test -f /interfaces.py && ln -s /labcontainers-guest /cldt-guest
ENTRYPOINT ["uv", "run", "/labcontainers-launch.py"]
