# Compile with controller/build-binaries.sh before assembling images.
# Pass --build-context binaries=<output-directory> and --platform=linux/<arch>.
# No target-architecture programs run during image assembly.
FROM gcr.io/distroless/static-debian12:nonroot AS endpoint-controller
ARG TARGETARCH
COPY --from=binaries /linux/${TARGETARCH}/endpoint-controller /endpoint-controller
COPY --from=binaries /linux/${TARGETARCH}/platform-tuple /platform-tuple
# join.Reconciler's default --join-template-path; the join-pattern
# templates are versioned alongside this Dockerfile's own repo, not a
# separate distribution mechanism.
COPY join-patterns /join-patterns
COPY --from=binaries /dialer-manifest.json /join-patterns/dialer-manifest.json
USER nonroot:nonroot
ENTRYPOINT ["/endpoint-controller"]

# Runs hostNetwork with CAP_NET_ADMIN to manage wg0 directly via netlink --
# needs root, unlike endpoint-controller which only talks to the API server.
FROM gcr.io/distroless/static-debian12:latest AS dialer
ARG TARGETARCH
COPY --from=binaries /linux/${TARGETARCH}/dialer /dialer
ENTRYPOINT ["/dialer"]
