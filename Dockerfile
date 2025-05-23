FROM golang:1.23.1 AS builder
WORKDIR /app
COPY . .
RUN go mod init bomb || true
RUN go build -o main .

FROM gcr.io/distroless/base
WORKDIR /
COPY --from=builder /app/main /main
COPY --from=builder /app/index.html /index.html
COPY --from=builder /app/static /static
CMD ["/main"]