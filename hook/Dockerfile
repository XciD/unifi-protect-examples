# Base image
FROM golang:1.16 as base

ENV GO111MODULE=on

WORKDIR /go/src

# Builder image
FROM base as builder
COPY . .

RUN CGO_ENABLED=0 go build -o hook main.go

# Release image
FROM alpine as release

ADD config.yaml /

COPY --from=builder /go/src/hook /hook

ENTRYPOINT /hook
