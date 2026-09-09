FROM vrnetlab/canonical_ubuntu:jammy
COPY guestagent /cldt-guest
COPY launcher.py /cldt-launch.py
ENTRYPOINT ["uv", "run", "/cldt-launch.py"]
