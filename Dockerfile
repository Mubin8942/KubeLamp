# KubeLamp — all-in-one image.
#
# One image for every setup: the KubeLamp binary plus the programs some kubeconfigs and
# features need at run time:
#   kubectl  — the local shell in the Terminal panel (needs -allow-shell beyond localhost)
#   aws      — EKS kubeconfigs that log in with `aws eks get-token`
#   bash, curl, jq — a usable shell for the Terminal panel
# Non-root (uid 65532), works with a read-only root filesystem.
#
#   docker build -t kubelamp .
#   docker run -p 7080:7080 --user "$(id -u):$(id -g)" -v ~/.kube:/kube:ro -v kubelamp-data:/data kubelamp
#
# The Go build runs on the build machine's own architecture and cross-compiles (no emulation),
# so `docker buildx build --platform linux/amd64,linux/arm64` is fast.
ARG GO_VERSION=1.22
ARG ALPINE_VERSION=3.20

# ---------------------------------------------------------------- build
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
# dependencies first so they are cached between builds
COPY go.mod go.sum* ./
COPY third_party ./third_party
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/kubelamp .

# ---------------------------------------------------------------- runtime
FROM alpine:${ALPINE_VERSION}
ARG VERSION=dev
LABEL org.opencontainers.image.title="KubeLamp" \
      org.opencontainers.image.description="Lightweight multi-cluster Kubernetes dashboard with terminals and pod tools (kubectl and AWS CLI included)" \
      org.opencontainers.image.source="https://github.com/Mubin8942/KubeLamp" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
RUN apk add --no-cache ca-certificates tzdata bash curl jq kubectl aws-cli \
 && addgroup -g 65532 kubelamp \
 && adduser -D -u 65532 -G kubelamp -h /home/kubelamp -s /bin/bash kubelamp \
 && mkdir -p /data /kube \
 && chown 65532:65532 /data /kube \
 && chmod 1777 /data /home/kubelamp
# /data and $HOME are writable for any user id, so `docker run --user $(id -u):$(id -g)` works:
# that is needed to read a kubeconfig that is only readable by you (mode 600).
COPY --from=build /out/kubelamp /usr/local/bin/kubelamp
ENV HOME=/home/kubelamp \
    KUBELAMP_ADDR=0.0.0.0:7080 \
    KUBELAMP_NO_BROWSER=true \
    KUBELAMP_DATA=/data \
    KUBECONFIG=/kube/config
USER 65532:65532
WORKDIR /home/kubelamp
EXPOSE 7080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD ["/usr/local/bin/kubelamp", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/kubelamp"]
