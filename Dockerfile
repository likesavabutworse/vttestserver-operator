# Release images. Build one target for each image:
#   docker buildx build --target manager --platform linux/amd64,linux/arm64 -t <registry>/vttestserver-operator:<tag> .
#   docker buildx build --target datactl --platform linux/amd64,linux/arm64 -t <registry>/vttestserver-operator-datactl:<tag> .
# The Go compiler runs on the build host and cross compiles, so a multi-platform build needs no emulation.
# Override BASE_IMAGE or RUNTIME_IMAGE to build from another registry.
ARG BASE_IMAGE=golang:1.26@sha256:672c944cfd07599b5f1bb179869270cdc94c6254e41db4d431259693cb286b82
ARG RUNTIME_IMAGE=gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go modules manifests first, so that source changes do not invalidate the download layer.
COPY go.mod go.sum ./
RUN go mod download

# .dockerignore filters the source files.
COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/datactl ./cmd/datactl

FROM ${RUNTIME_IMAGE} AS manager
WORKDIR /
COPY --from=builder /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]

FROM ${RUNTIME_IMAGE} AS datactl
COPY --from=builder /out/datactl /datactl
# The user of the official vttestserver image. It owns the data dir.
USER 999:999
ENTRYPOINT ["/datactl"]
