#!/usr/bin/env python3
"""Read-only admission check; run on the host, not inside a runner."""
import argparse
import json
import os
from pathlib import Path
import shutil


def evaluate(memory_gib, free_gib, pressure, requested_gib, reserve_gib, disk_gib):
    failures = []
    if memory_gib < requested_gib + reserve_gib:
        failures.append("insufficient available RAM (swap is not lab capacity)")
    if free_gib < disk_gib:
        failures.append("insufficient free space on the state filesystem")
    for name, limit in (("memory", 1), ("io", 5), ("cpu", 20)):
        if pressure[name] > limit:
            failures.append(f"{name} pressure exceeds {limit}% over the last 10 seconds")
    return failures


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state", type=Path, required=True, help="existing state directory on the intended disk")
    parser.add_argument("--additional-memory-gib", type=float, default=0, help="RAM for new VMs, daemons and build buffers; zero for an existing lab")
    parser.add_argument("--reserve-memory-gib", type=float, default=8)
    parser.add_argument("--free-disk-gib", type=float, default=100)
    args = parser.parse_args()
    if any(x < 0 for x in (args.additional_memory_gib, args.reserve_memory_gib, args.free_disk_gib)):
        parser.error("resource budgets must be nonnegative")
    if Path("/.dockerenv").exists():
        parser.error("run this check on the host, outside Docker")
    memory = dict(line.split(":", 1) for line in Path("/proc/meminfo").read_text().splitlines())
    available = int(memory["MemAvailable"].split()[0]) / 1024**2
    pressure = {}
    for name in ("memory", "io", "cpu"):
        line = next(line for line in Path(f"/proc/pressure/{name}").read_text().splitlines() if line.startswith("some "))
        pressure[name] = float(dict(field.split("=") for field in line.split()[1:])["avg10"])
    free = shutil.disk_usage(args.state.resolve(strict=True)).free / 1024**3
    failures = evaluate(available, free, pressure, args.additional_memory_gib, args.reserve_memory_gib, args.free_disk_gib)
    print(json.dumps({"availableMemoryGiB": available, "freeStateDiskGiB": free,
                      "pressureAvg10Percent": pressure, "cpuCount": os.cpu_count(),
                      "failures": failures, "passed": not failures}, indent=2))
    return bool(failures)


if __name__ == "__main__":
    raise SystemExit(main())
