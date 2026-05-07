FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod .
COPY main.go .
RUN go build -o pod-resize-demo .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/pod-resize-demo .
EXPOSE 8080
ENTRYPOINT ["./pod-resize-demo"]
