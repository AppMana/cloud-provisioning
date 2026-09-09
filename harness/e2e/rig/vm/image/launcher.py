#!/usr/bin/env python3
"""The Ubuntu vrnetlab launcher with one data NIC and a serial guest agent."""
import argparse
import importlib.util
import logging
from pathlib import Path
import subprocess

spec = importlib.util.spec_from_file_location("ubuntu_launcher", "/launch.py")
ubuntu = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ubuntu)


class SingleNIC(ubuntu.Ubuntu_vm):
    def __init__(self, hostname):
        super().__init__(hostname, "sysadmin", "sysadmin", 1, "tc")
        self.qemu_args.extend([
            "-chardev", "socket,path=/run/cldt-qga.sock,server=on,wait=off,id=qga0",
            "-device", "virtio-serial-pci,id=serial1",
            "-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
        ])

    def gen_mgmt(self):
        return []


class LabVM(ubuntu.vrnetlab.VR):
    def __init__(self, hostname):
        super().__init__("sysadmin", "sysadmin")
        self.vms = [SingleNIC(hostname)]


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--hostname", required=True)
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO)
    subprocess.Popen(["/cldt-guest", "serve"])
    reset = Path("/cldt-reset-instance")
    if reset.exists():
        for disk in Path("/").glob("*-overlay.qcow2"):
            disk.unlink()
        reset.unlink()
    LabVM(args.hostname).start()
