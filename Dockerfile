# Build stage
FROM golang:1.21-alpine AS builder

# Install build dependencies
RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o telegram-bot main_voice.go

# Runtime stage
FROM alpine:latest

# Install runtime dependencies
RUN apk add --no-cache ca-certificates sqlite-libs

WORKDIR /root/

# Copy binary from builder
COPY --from=builder /app/telegram-bot .

# Create directory for database
RUN mkdir -p /root/data

# Set environment variable for database path
ENV DB_FILE=/root/data/bot_history.db

# Run the bot
CMD ["./telegram-bot"]
