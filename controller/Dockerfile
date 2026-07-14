# Two images from one module, selected with --target:
#   docker build --target endpoint-controller -t <tag> .
#   docker build --target dialer -t <tag> .
#
# --platform=$BUILDPLATFORM pins the build stage to the runner's own
# architecture regardless of the requested target --platform, so a
# multi-arch build cross-compiles natively (Go itself needs no QEMU to
# target a different GOARCH) instead of running the entire Go toolchain
# under emulation for arm64, which is what happens without this.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY controller/go.mod controller/go.sum ./
RUN go mod download
COPY controller/cmd ./cmd
COPY controller/pkg ./pkg
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/endpoint-controller ./cmd/endpoint-controller
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/dialer ./cmd/dialer

FROM gcr.io/distroless/static-debian12:nonroot AS endpoint-controller
COPY --from=build /out/endpoint-controller /endpoint-controller
# join.Reconciler's default --join-template-path; the join-pattern
# templates are versioned alongside this Dockerfile's own repo, not a
# separate distribution mechanism.
COPY join-patterns /join-patterns
USER nonroot:nonroot
ENTRYPOINT ["/endpoint-controller"]

# Runs hostNetwork with CAP_NET_ADMIN to manage wg0 directly via netlink --
# needs root, unlike endpoint-controller which only talks to the API server.
FROM gcr.io/distroless/static-debian12:latest AS dialer
COPY --from=build /out/dialer /dialer
ENTRYPOINT ["/dialer"]
