#syntax=docker/dockerfile:1.2
FROM --platform=$BUILDPLATFORM golang:1.19.4@sha256:1a90b7fdf1fa9f207301f1afe8aa0bbb0fa3d1a0df5674a34494f50133c5ffc3 AS builder

WORKDIR /build

ARG GOPROXY
ENV GO111MODULE=on
ENV CGO_ENABLED=0

COPY go.mod go.sum ./
RUN go mod download

ARG TARGETOS
ARG TARGETARCH

ENV GOOS=${TARGETOS}
ENV GOARCH=${TARGETARCH}

COPY . ./
RUN \
    --mount=type=cache,id=gomod,target=/go/pkg/mod \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    make all BINDIR=/out

RUN pwd; find /build /out -type  f -ls

RUN chown -R root:root /out
RUN chmod -R u=rwX,go=rX /out

FROM --platform=$BUILDPLATFORM debian:stable@sha256:880aa5f5ab441ee739268e9553ee01e151ccdc52aa1cd545303cfd3d436c74db AS cniplugins

RUN \
    set -e; \
    rm -f /etc/apt/apt.conf.d/docker-clean; \
    echo 'Binary::apt::APT::Keep-Downloaded-Packages "true";' > /etc/apt/apt.conf.d/keep-cache

RUN \
    --mount=type=cache,id=apt,target=/var/cache/apt \
    --mount=type=cache,id=apt,target=/var/lib/apt \
    set -e; \
    apt-get update -qy; \
    apt-get install -qy wget

# renovate: datasource=github-releases depName=containernetworking/plugins 
ENV CNIPLUGINS_VERSION=v1.0.1

WORKDIR /build

ARG TARGETOS
ARG TARGETARCH

RUN \
    wget https://github.com/containernetworking/plugins/releases/download/${CNIPLUGINS_VERSION}/cni-plugins-${TARGETOS}-${TARGETARCH}-${CNIPLUGINS_VERSION}.tgz

RUN mkdir -p /out
RUN \
    tar zxv \
    -C /out \
    -f cni-plugins-${TARGETOS}-${TARGETARCH}-${CNIPLUGINS_VERSION}.tgz

RUN chown -R root:root /out
RUN chmod -R u=rwX,go=rX /out

FROM alpine:3.17.0@sha256:8914eb54f968791faf6a8638949e480fef81e697984fba772b3976835194c6d4

COPY --from=cniplugins /out/* /usr/local/bin/
COPY --from=builder /out/* /usr/local/bin/
