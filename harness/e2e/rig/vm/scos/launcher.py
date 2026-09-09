#!/usr/bin/env python3
"""One data NIC and serial QGA for SCOS disks or an agent-installer ISO."""
import os
from pathlib import Path
import re
import subprocess
import time


def run(*args):
    subprocess.run(args, check=True)


def main():
    state = Path("/state")
    state.mkdir(mode=0o700, exist_ok=True)
    disk = state / "disk.qcow2"
    if not disk.exists():
        if Path("/scos-base.qcow2").is_file():
            run("qemu-img", "create", "-f", "qcow2", "-F", "qcow2",
                "-b", "/scos-base.qcow2", str(disk), "120G")
        else:
            run("qemu-img", "create", "-f", "qcow2", str(disk), "120G")

    # Containerlab supplies eth1. This bridge has no address; it only carries
    # the guest's sole data link. No slirp, guest management NIC or SSH path.
    deadline = time.monotonic() + 120
    while not Path("/sys/class/net/eth1").exists():
        if time.monotonic() > deadline:
            raise RuntimeError("containerlab did not supply eth1")
        time.sleep(0.2)
    run("ip", "link", "add", "cldt-data", "type", "bridge")
    run("ip", "tuntap", "add", "tap0", "mode", "tap")
    for device in ("eth1", "tap0"):
        run("ip", "link", "set", device, "master", "cldt-data")
        run("ip", "link", "set", device, "up")
    run("ip", "link", "set", "cldt-data", "up")

    mac = os.environ["CLDT_MAC"]
    if not re.fullmatch(r"(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}", mac):
        raise ValueError("invalid CLDT_MAC")
    qemu = [
        "qemu-system-x86_64", "-enable-kvm", "-nodefaults", "-display", "none",
        "-machine", "pc", "-cpu", "host",
        "-m", str(int(os.environ.get("QEMU_MEMORY", "16384"))),
        "-smp", str(int(os.environ.get("QEMU_SMP", "4"))),
        "-drive", f"if=none,id=os,file={disk},format=qcow2",
        "-device", "virtio-blk-pci,drive=os,addr=0x4",
        "-netdev", "tap,id=data,ifname=tap0,script=no,downscript=no",
        "-device", f"virtio-net-pci,netdev=data,mac={mac},addr=0x2",
        "-chardev", "socket,path=/run/cldt-qga.sock,server=on,wait=off,id=qga0",
        "-device", "virtio-serial-pci,id=serial1,addr=0x3",
        "-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
        "-serial", "file:/state/console.log",
    ]
    if Path("/agent.iso").is_file() and not (state / "installed").exists():
        # Guest reboot after installation returns to disk. The rig records
        # installation completion before a later wrapper power-cycle.
        qemu += ["-cdrom", "/agent.iso", "-boot", "order=c,once=d"]
    if Path("/seed/config.ign").is_file():
        qemu += ["-fw_cfg", "name=opt/com.coreos/config,file=/seed/config.ign"]
    subprocess.Popen(["/cldt-guest", "serve"])
    os.execvp(qemu[0], qemu)


if __name__ == "__main__":
    main()
