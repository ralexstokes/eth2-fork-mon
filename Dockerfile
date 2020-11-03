FROM golang:alpine as build

RUN apk add --no-cache ca-certificates build-base

WORKDIR /build

ADD . .

RUN CGO_ENABLED=1 GOOS=linux \
    go build -ldflags '-extldflags "-static"' -o app cmd/eth2-fork-mon/main.go

FROM golang:alpine

COPY --from=build /etc/ssl/certs/ca-certificates.crt \
     /etc/ssl/certs/ca-certificates.crt

COPY --from=build /build/app /app

COPY --from=build /build/public /public

WORKDIR /

ENTRYPOINT ["/app"]
