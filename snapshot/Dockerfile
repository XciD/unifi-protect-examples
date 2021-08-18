# Base image
FROM golang:1.16 as base

ENV GO111MODULE=on

WORKDIR /go/src

# Builder image
FROM base as builder
COPY . .

RUN CGO_ENABLED=0 go build -o snapshot main.go

# Release image
FROM collelog/ffmpeg:4.4-alpine-amd64 as release
ENV FFMPEG /usr/local/bin/ffmpeg

ADD config.yaml /

COPY --from=builder /go/src/snapshot /snapshot

ENTRYPOINT /snapshot
