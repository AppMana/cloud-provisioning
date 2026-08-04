#!/usr/bin/env python3
"""Log into the guest over its serial console (via the vrnetlab wrapper
container's own qemu monitor at localhost:5000) and run one command,
printing the output.

This bypasses the guest's own network stack entirely, so it works even
when the guest has hijacked its own default route and is unreachable
over SSH/kubectl. That lets the harness read the guest's routing table
directly, rather than inferring it from reachability.

Usage: serial-console.py <container-name> <command>
"""
import subprocess
import sys

GUEST_SCRIPT = """
import socket, time, sys

def read(s, t=3):
    s.settimeout(t)
    buf = b''
    try:
        while True:
            chunk = s.recv(4096)
            if not chunk:
                break
            buf += chunk
    except socket.timeout:
        pass
    return buf

s = socket.create_connection(('localhost', 5000), timeout=10)
read(s, 3)
s.sendall(b'\\r\\n')
time.sleep(1)
read(s, 2)
s.sendall(b'sysadmin\\r\\n')
time.sleep(2)
read(s, 3)
s.sendall(b'sysadmin\\r\\n')
time.sleep(3)
read(s, 3)
s.sendall(({command!r} + '\\r\\n').encode())
time.sleep(3)
sys.stdout.buffer.write(read(s, 3))
"""


def main():
    if len(sys.argv) != 3:
        print("usage: serial-console.py <container-name> <command>", file=sys.stderr)
        return 1
    container, command = sys.argv[1], sys.argv[2]
    script = GUEST_SCRIPT.format(command=command)
    result = subprocess.run(
        ["docker", "exec", "-i", container, "python3", "-c", script],
        capture_output=True, text=True, timeout=40,
    )
    print(result.stdout)
    if result.returncode != 0:
        print(result.stderr, file=sys.stderr)
    return result.returncode


if __name__ == "__main__":
    sys.exit(main())
