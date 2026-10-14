ARG LABCONTAINERS_VM_IMAGE
FROM ${LABCONTAINERS_VM_IMAGE}
ARG SOURCE_REVISION
COPY labcontainers-guest /labcontainers-guest
COPY launcher.py /labcontainers-launch.py
COPY interfaces.py /interfaces.py
RUN test -n "$SOURCE_REVISION" && test -x /labcontainers-guest && ln -sf /labcontainers-guest /cldt-guest
LABEL appmana.labcontainers.revision="${SOURCE_REVISION}"
ENTRYPOINT ["uv", "run", "/labcontainers-launch.py"]
