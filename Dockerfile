# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
# `go mod tidy` generates go.sum on first build. Once you've committed go.sum,
# replace it with `go mod download` above the COPY for better layer caching.
RUN go mod tidy \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rateguard ./cmd/rateguard \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/loadtest  ./cmd/loadtest

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/rateguard /out/loadtest /usr/local/bin/
USER app
EXPOSE 8080
ENTRYPOINT ["rateguard"]
