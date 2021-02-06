FROM golang:1.15-alpine AS builder

WORKDIR /go/src/github.com/openfaas-incubator/faas-idler

ENV GO111MODULE=on
ENV CGO_ENABLED=0

COPY types      types
COPY main.go    main.go
COPY vendor     vendor
COPY go.mod     go.mod
COPY go.sum     go.sum

RUN go build -mod=vendor -o /usr/bin/faas-idler .

FROM alpine:3.13

RUN addgroup -S faas && adduser -S -g faas faas

COPY --from=builder /usr/bin/faas-idler /usr/bin/faas-idler

RUN chown -R faas /usr/bin/faas-idler
USER faas

EXPOSE 8080
VOLUME /tmp

ENTRYPOINT ["/usr/bin/faas-idler"]
