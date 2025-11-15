FROM golang:1.24-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o firerunner ./cmd/firerunner

FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY --from=builder /build/firerunner /usr/local/bin/firerunner

RUN addgroup -S firerunner && adduser -S firerunner -G firerunner

USER firerunner

EXPOSE 8080 9090

ENTRYPOINT ["/usr/local/bin/firerunner"]
CMD ["--config", "/etc/firerunner/config.yaml"]
