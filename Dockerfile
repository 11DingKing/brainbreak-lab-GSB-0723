FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=local go mod download

COPY . .
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -ldflags="-s -w" -o /bin/brainbreak-server ./cmd/server/

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -u 10001 brainbreak

COPY --from=builder /bin/brainbreak-server /usr/local/bin/brainbreak-server
COPY migrations/ /app/migrations/

WORKDIR /app
USER brainbreak

EXPOSE 8080

ENV SERVER_ADDR=:8080

ENTRYPOINT ["brainbreak-server"]
