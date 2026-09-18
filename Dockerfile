FROM golang:1.24-alpine AS build

WORKDIR /poker

RUN apk --update add ca-certificates upx

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -ldflags="-w -s" -o /bin/poker main.go

RUN upx -v /bin/poker


FROM scratch

WORKDIR /

# The public liteserver pool is fetched over HTTPS from ton.org, so the root certificates have to
# come along; a scratch image without them fails at startup with an x509 error and falls back to
# own nodes only, which is the configuration this service exists to not depend on.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /bin/poker /poker

EXPOSE 10000

ENTRYPOINT ["/poker"]
