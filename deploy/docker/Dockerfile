# syntax=docker/dockerfile:1

# CGO stays off: the static runtime image has no libc.
FROM golang:1.26.9-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gatewayctl ./cmd/gatewayctl && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fakeprovider ./cmd/fakeprovider

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /out/gatewayctl /out/fakeprovider /usr/local/bin/
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/gateway"]
