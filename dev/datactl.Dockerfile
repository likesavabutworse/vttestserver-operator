# Dev image for Tilt. The binary is cross compiled on the host (see Tiltfile).
FROM gcr.io/distroless/static:nonroot
COPY datactl_linux /datactl
USER 999:999
ENTRYPOINT ["/datactl"]
