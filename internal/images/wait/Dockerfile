# Keep the builder toolchain current: scratch does not remove Go runtime vulnerabilities.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-alpine@sha256:c95332c2af86b6d89b91bd0500f4b9529ccbd090a0d1855c6d1ceaa142ae8615 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOTOOLCHAIN=local GOPROXY=off go build -trimpath -ldflags="-s -w" -o /wait main.go

FROM scratch
COPY --from=build /wait /wait
USER 65532:65532
ENTRYPOINT ["/wait"]
