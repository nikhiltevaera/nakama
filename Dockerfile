# Stage 1: Build the Nakama server
FROM golang:1.22 as builder
ENV GO111MODULE on
ENV CGO_ENABLED 1

WORKDIR /app
COPY . .

RUN go build -o ./nakama .

# Stage 2: Create the final image
FROM alpine:latest

COPY --from=builder /app/nakama /nakama/
COPY --from=builder /app/config.yml /nakama/data/

