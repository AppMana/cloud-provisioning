FROM ubuntu:24.04@sha256:c35e29c9450151419d9448b0fd75374fec4fff364a27f176fb458d472dfc9e54 AS build
RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev libvulkan-dev
COPY render.c /src/render.c
RUN gcc -std=c11 -Wall -Wextra -Werror -O2 /src/render.c -lvulkan -o /gpu-render
FROM ubuntu:24.04@sha256:c35e29c9450151419d9448b0fd75374fec4fff364a27f176fb458d472dfc9e54
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg libvulkan1 libegl1 && rm -rf /var/lib/apt/lists/*
COPY --from=build /gpu-render /usr/local/bin/gpu-render
COPY run.sh /usr/local/bin/gpu-probe
ENTRYPOINT ["/bin/sh", "/usr/local/bin/gpu-probe"]
