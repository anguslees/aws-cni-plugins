#syntax=docker/dockerfile:1.2
FROM --platform=$BUILDPLATFORM golang:1.19.2@sha256:b850621230956a6d960d6d7cfaba6a8a2e8e245b230a928ef66aa0cfd065e229 AS builder

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

FROM --platform=$BUILDPLATFORM debian:stable@sha256:1b1d158efc589b1eb8858acdc95bd0ff4c5344958f834b7326662da8482b3e7d AS cniplugins

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

FROM alpine:3.15.4@sha256:4edbd2beb5f78b1014028f4fbb99f3237d9561100b6881aabbf5acce2c4f9454

COPY --from=cniplugins /out/* /usr/local/bin/
COPY --from=builder /out/* /usr/local/bin/
