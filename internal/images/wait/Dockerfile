# Keep the builder toolchain current: scratch does not remove Go runtime vulnerabilities.
FROM --platform=$BUILDPLATFORM mirror.gcr.io/library/golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY cmd/main.go .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOTOOLCHAIN=local GOPROXY=off go build -trimpath -ldflags="-s -w" -o /wait main.go

FROM scratch
COPY --from=build /wait /wait
USER 65532:65532
ENTRYPOINT ["/wait"]
