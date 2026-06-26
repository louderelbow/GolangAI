FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 用 Docker 配置（hostname 指向 docker-compose 服务名）
COPY config/config.toml.docker config/config.toml
RUN CGO_ENABLED=0 go build -o deeptalk .

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/deeptalk .
COPY --from=builder /app/config ./config
RUN mkdir -p uploads
EXPOSE 9090
CMD ["./deeptalk"]
