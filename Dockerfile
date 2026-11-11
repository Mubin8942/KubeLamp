ARG GO_VERSION=1.22
ARG ALPINE_VERSION=3.20

# ---------------------------------------------------------------- build
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS TARGETARCH
# trust Robi proxy CA so go mod download works
COPY proxy-ca.crt /usr/local/share/ca-certificates/proxy-ca.crt
RUN cat /usr/local/share/ca-certificates/proxy-ca.crt >> /etc/ssl/certs/ca-certificates.crt
WORKDIR /src
COPY go.mod go.sum* ./
COPY third_party ./third_party
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/kubelamp .

# ---------------------------------------------------------------- runtime
FROM alpine:${ALPINE_VERSION}
ARG VERSION=dev
LABEL org.opencontainers.image.title="KubeLamp" \
      org.opencontainers.image.source="https://github.com/Mubin8942/KubeLamp" \
      org.opencontainers.image.version="${VERSION}"
# trust Robi proxy CA before apk, then make it permanent after ca-certificates is installed
COPY proxy-ca.crt /usr/local/share/ca-certificates/proxy-ca.crt
RUN cat /usr/local/share/ca-certificates/proxy-ca.crt >> /etc/ssl/certs/ca-certificates.crt \
 && apk add --no-cache ca-certificates tzdata bash curl jq kubectl aws-cli \
 && update-ca-certificates \
 && addgroup -g 65532 kubelamp \
 && adduser -D -u 65532 -G kubelamp -h /home/kubelamp -s /bin/bash kubelamp \
 && mkdir -p /data /kube \
 && chown 65532:65532 /data /kube \
 && chmod 1777 /data /home/kubelamp
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
