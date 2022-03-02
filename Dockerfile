#syntax=docker/dockerfile:1.2
FROM --platform=$BUILDPLATFORM golang:1.17.7@sha256:b983574376a29f21ccba0a90f1e1f31864d084cee73295b774305b792b31b8bd AS builder

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

FROM --platform=$BUILDPLATFORM debian:stable@sha256:794b8d9c2c8e5aef02e1599859de96c26d9558619cc69067e43c07d583154ef5 AS cniplugins

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

FROM alpine:3.15.0@sha256:21a3deaa0d32a8057914f36584b5288d2e5ecc984380bc0118285c70fa8c9300

COPY --from=cniplugins /out/* /usr/local/bin/
COPY --from=builder /out/* /usr/local/bin/
