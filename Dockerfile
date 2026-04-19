FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY backend/ .
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o console-manager .

FROM alpine:3.19
RUN apk add --no-cache bash
WORKDIR /app
COPY --from=builder /app/console-manager .
EXPOSE 8080
CMD ["./console-manager"]
