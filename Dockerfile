FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/shhttpd ./cmd/shhttpd

# Alpine rather than distroless: sessions need a shell and basic tools.
FROM alpine:3
RUN adduser -D -u 10001 shhttp && mkdir /data && chown shhttp /data
COPY --from=build /out/shhttpd /usr/local/bin/shhttpd
USER shhttp
ENV SHHTTP_LISTEN=0.0.0.0:2112 SHHTTP_DATA_DIR=/data
VOLUME /data
EXPOSE 2112
ENTRYPOINT ["/usr/local/bin/shhttpd"]
