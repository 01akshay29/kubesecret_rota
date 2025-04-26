# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.22 AS builder

WORKDIR /app
COPY . .

# Build the Go binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -o secret-watcher main.go

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/
COPY --from=builder /app/secret-watcher .

ENTRYPOINT ["./secret-watcher"]
