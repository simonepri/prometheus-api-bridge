FROM golang:1.26.6-alpine3.23 AS build

WORKDIR /src

COPY go.mod go.sum ./
COPY bridge ./bridge

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/prometheus-api-bridge \
    ./bridge

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/prometheus-api-bridge /prometheus-api-bridge

USER 65532:65532
EXPOSE 9090
ENTRYPOINT ["/prometheus-api-bridge"]
