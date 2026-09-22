FROM vrnetlab/canonical_ubuntu:jammy
COPY guestagent /labcontainers-guest
RUN ln -s /labcontainers-guest /cldt-guest
COPY launcher.py /cldt-launch.py
ENTRYPOINT ["uv", "run", "/cldt-launch.py"]
