# KubeLamp — slim image: static binary on distroless, non-root, no shell.
#   docker run -p 7080:7080 -v ~/.kube:/kube:ro -v kubelamp-data:/data ghcr.io/zanayeem/kubelamp
# The Go build runs on the build machine's own architecture and cross-compiles (no emulation).
ARG GO_VERSION=1.22

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/kubelamp . \
 && mkdir -p /out/data /out/kube && chmod 1777 /out/data
# /data is writable for any user id, so `docker run --user $(id -u):$(id -g)` works: that is
# needed to read a kubeconfig that is only readable by you (mode 600).

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="KubeLamp" \
      org.opencontainers.image.description="Lightweight multi-cluster Kubernetes dashboard with terminals and pod tools" \
      org.opencontainers.image.source="https://github.com/zanayeem/KubeLamp" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/kubelamp /usr/local/bin/kubelamp
COPY --from=build --chown=65532:65532 --chmod=1777 /out/data /data
COPY --from=build --chown=65532:65532 /out/kube /kube
ENV KUBELAMP_ADDR=0.0.0.0:7080 \
    KUBELAMP_NO_BROWSER=true \
    KUBELAMP_DATA=/data \
    KUBECONFIG=/kube/config
USER 65532:65532
EXPOSE 7080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD ["/usr/local/bin/kubelamp", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/kubelamp"]
