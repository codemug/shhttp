FROM golang:1.11-alpine

RUN apk add --no-cache --update git && \
    go get github.com/golang/dep/cmd/dep && \
    mkdir -p /go/src/shhttp

WORKDIR /go/src/shhttp

ADD *.go Gopkg.* ./

RUN dep ensure && \
    go install

FROM alpine:3.9

COPY --from=0 /go/bin/shhttp /usr/bin/

ENTRYPOINT [ "/usr/bin/shhttp" ]