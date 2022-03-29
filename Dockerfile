#syntax=docker/dockerfile:1.2
FROM --platform=$BUILDPLATFORM golang:1.17.8@sha256:dba09c285770f58d6685b25a0606d72420b0a7525a2338080807d138a258c671 AS builder

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

FROM --platform=$BUILDPLATFORM debian:stable@sha256:1c3446475ac28a9f42a4627d8945d7bed88b8128b5850b61c5890ff47f317681 AS cniplugins

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

FROM alpine:3.15.3@sha256:f22945d45ee2eb4dd463ed5a431d9f04fcd80ca768bb1acf898d91ce51f7bf04

COPY --from=cniplugins /out/* /usr/local/bin/
COPY --from=builder /out/* /usr/local/bin/
