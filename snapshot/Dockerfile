# Base image
FROM golang:1.16 as base

ENV GO111MODULE=on

WORKDIR /go/src

# Builder image
FROM base as builder
COPY . .

RUN go build -o snapshot main.go

# Release image
FROM jrottenberg/ffmpeg:4.1-ubuntu as release

WORKDIR /

ENV FFMPEG /usr/local/bin/ffmpeg

ADD config.yaml /

COPY --from=builder /go/src/snapshot /snapshot

ENTRYPOINT /snapshot
