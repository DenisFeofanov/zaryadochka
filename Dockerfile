FROM alpine:latest

# Install sqlite
RUN apk add --no-cache sqlite-libs

WORKDIR /app

# Copy pre-built binary
COPY main .
# Copy .env file
COPY .env .

# Create volume for database
VOLUME ["/app/data"]

# Run the binary
CMD ["./main"]
