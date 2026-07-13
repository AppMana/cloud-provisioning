# Two images from one module, selected with --target:
#   docker build --target endpoint-controller -t <tag> .
#   docker build --target dialer -t <tag> .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /out/endpoint-controller ./cmd/endpoint-controller
RUN CGO_ENABLED=0 go build -o /out/dialer ./cmd/dialer

FROM gcr.io/distroless/static-debian12:nonroot AS endpoint-controller
COPY --from=build /out/endpoint-controller /endpoint-controller
USER nonroot:nonroot
ENTRYPOINT ["/endpoint-controller"]

# Runs hostNetwork with CAP_NET_ADMIN to manage wg0 directly via netlink --
# needs root, unlike endpoint-controller which only talks to the API server.
FROM gcr.io/distroless/static-debian12:latest AS dialer
COPY --from=build /out/dialer /dialer
ENTRYPOINT ["/dialer"]
