# Use Go 1.25.1 for building the autoscaler
FROM golang:1.25.1 AS builder

WORKDIR /app

# Copy go.mod and go.sum first (for caching dependencies)
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy all source code
COPY . .

# Build the binary
#RUN CGO_ENABLED=0 GOOS=linux go build -o autoscaler main.go
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o autoscaler ./main.go

# Use a minimal image for runtime
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /root/

# Copy the compiled binary from builder
COPY --from=builder /app/autoscaler .

# Command to run the autoscaler
CMD ["./autoscaler"]