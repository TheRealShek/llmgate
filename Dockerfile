# syntax=docker/dockerfile:1
FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/llmgate ./cmd/gateway

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 llmgate \
    && adduser -S -D -H -u 10001 -G llmgate llmgate
COPY --from=build /out/llmgate /usr/local/bin/llmgate
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/llmgate"]
