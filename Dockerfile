FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o server-sim ./cmd/server-sim

FROM alpine:3.20
WORKDIR /app
COPY --from=builder /app/server-sim .
EXPOSE 8080
ENTRYPOINT ["./server-sim"]
