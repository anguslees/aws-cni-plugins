#syntax=docker/dockerfile:1.2
FROM --platform=$BUILDPLATFORM golang:1.19.5@sha256:bace16d065b3733fabbe1d282286afbb0e1043e03c91c4cd8e0e6d319c46eb3e AS builder

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

FROM --platform=$BUILDPLATFORM debian:stable@sha256:cab1b92fc6a2c481a9ed4ca1a70e5df3bef0219e20fe70101c26322c09e5a3c4 AS cniplugins

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

FROM alpine:3.17.1@sha256:f271e74b17ced29b915d351685fd4644785c6d1559dd1f2d4189a5e851ef753a

COPY --from=cniplugins /out/* /usr/local/bin/
COPY --from=builder /out/* /usr/local/bin/
