# syntax=docker/dockerfile:1

# CGO stays off: the static runtime image has no libc.
FROM golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# .git is not in the build context, so the version comes in as an argument.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gatewayctl ./cmd/gatewayctl && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fakeprovider ./cmd/fakeprovider

# Test-only server, kept out of the production image below.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS fakeprovider
COPY --from=build /out/fakeprovider /usr/local/bin/
EXPOSE 8081
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/fakeprovider"]

# The last stage is the default target.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/gateway /out/gatewayctl /usr/local/bin/
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/gateway"]
