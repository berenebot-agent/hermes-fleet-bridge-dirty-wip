FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /bridge .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates docker-cli docker-cli-compose && mkdir -p /data
COPY --from=build /bridge /usr/local/bin/bridge
ENV DATA_DIR=/data
ENTRYPOINT ["/usr/local/bin/bridge"]
