# Use official golang image as builder
FROM golang:1.21-alpine AS builder

# Set working directory
WORKDIR /app

# Install sqlite and gcc
RUN apk add --no-cache gcc musl-dev sqlite-dev

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=1 GOOS=linux go build -o main .

# Use alpine for smaller final image
FROM alpine:latest

# Install sqlite
RUN apk add --no-cache sqlite-libs

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/main .
# Copy .env file
COPY .env .

# Create volume for database
VOLUME ["/app/data"]

# Run the binary
CMD ["./main"]
