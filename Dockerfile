FROM golang:1.25-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s" \
    -tags failpoint -gcflags=all=-l \
    -o stress-test \
    .

FROM alpine:latest

RUN apk add --no-cache tzdata

WORKDIR /app

COPY --from=builder /build/stress-test .

RUN adduser -D -u 1000 tester && \
    chown -R tester:tester /app/stress-test

USER tester

ENTRYPOINT ["/app/stress-test"]
