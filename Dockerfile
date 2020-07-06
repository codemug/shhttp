FROM golang:1.14-alpine

RUN mkdir -p /go/src/github.com/codemug/shhttp && \
    apk update && apk add alpine-sdk

ADD . /go/src/github.com/codemug/shhttp

RUN cd /go/src/github.com/codemug/shhttp && \
    go mod tidy && \
    go test github.com/codemug/shhttp/pkg -v && \
    go build -o /go/bin/shhttp github.com/codemug/shhttp/cmd/shhttp

FROM alpine:3.11

COPY --from=0 /go/bin/shhttp /usr/local/bin/shhttp

ENTRYPOINT ["/usr/local/bin/shhttp"]