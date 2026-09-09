# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32

FROM --platform=$BUILDPLATFORM golang:1.27@sha256:0ecdc2a9f6156af6451080bfe3d8382a662fcc4e209608c6f919e643453514c1 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
    GOOS="${TARGETOS}" \
    GOARCH="${TARGETARCH}" \
    go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w -buildid=" \
      -o /transform \
      .

FROM scratch

LABEL com.docker.compose.bridge=transformation \
      org.opencontainers.image.source="https://github.com/defenseunicorns-labs/compose-bridge-uds" \
      org.opencontainers.image.description="Transform Docker Compose applications into deployable UDS packages." \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder \
     --chown=65532:65532 \
     --chmod=0555 \
     /transform /transform

USER 65532:65532

ENTRYPOINT ["/transform"]
