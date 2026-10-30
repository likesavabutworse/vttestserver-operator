# Dev image for Tilt. The binary is cross compiled on the host (see Tiltfile).
# Release images use the distroless Dockerfile in the repo root.
FROM alpine:3.21
COPY manager_linux /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
