FROM cloud-provisioning/vm:single-nic
COPY launcher.py /cldt-scos.py
HEALTHCHECK --interval=30s --timeout=10s --start-period=120s CMD ["/cldt-guest", "exec", "5s", "0", "/etc/qemu-ga/fsfreeze-hook.d/cldt-exec", "true"]
ENTRYPOINT ["python3", "/cldt-scos.py"]
