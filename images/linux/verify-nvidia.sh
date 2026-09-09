#!/bin/sh
# Host acceptance only; the scheduled render/encode workload is a separate gate.
set -eu
nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv
# Fail on an unavailable encoder even if enumeration succeeds.
ffmpeg -hide_banner -f lavfi -i testsrc2=size=640x360:rate=30 \
  -frames:v 30 -c:v h264_nvenc -f null -
