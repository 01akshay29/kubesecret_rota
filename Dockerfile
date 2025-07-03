FROM golang:1.24 AS builder
WORKDIR /app
COPY . .
RUN go mod tidy && go build -o secret-checker

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/secret-checker /
ENTRYPOINT ["/secret-checker"]