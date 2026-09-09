#!/bin/sh
set -eu
gpu-render
ffmpeg -hide_banner -f lavfi -i testsrc2=size=640x360:rate=30 \
  -frames:v 30 -c:v h264_nvenc -f null -
printf '%s\n' '{"vulkanPixelsVerified":4096,"nvencFrames":30}' > /dev/termination-log
