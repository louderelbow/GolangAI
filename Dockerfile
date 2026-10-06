# ============================================================
# 后端镜像
#
# 分两阶段：构建需要完整的 Go 工具链（几百 MB），运行只需要一个静态二进制。
# 分阶段之后最终镜像只有 alpine + 一个可执行文件，体积小很多、攻击面也小。
# ============================================================

# ---------- 阶段 1：编译 ----------
FROM golang:1.25-alpine AS builder

WORKDIR /app

# 先只拷依赖清单，让 go mod download 这层能被 Docker 缓存：
# 只要 go.mod/go.sum 没变，改业务代码不会重新下载依赖
COPY go.mod go.sum ./
RUN go mod download

# 再拷源码
COPY . .

# 把容器专用配置（服务名代替 127.0.0.1）作为镜像里的默认配置。
# 运行时 docker-compose 还会把宿主机的 config/config.toml.docker 挂载到同一路径覆盖它，
# 这样改配置不用重新构建。
COPY config/config.toml.docker config/config.toml

# CGO_ENABLED=0：产出静态链接的二进制，不依赖系统 glibc/libc，可以直接跑在 alpine 上。
#
# 注意这里**故意不加** -ldflags="-s -w"：那两个参数会剥掉符号表和调试信息，
# 二进制会小一些，但 pprof 就再也看不到函数名了（profile 里全是地址）。
# 排查性能问题的能力比省几 MB 重要。
RUN CGO_ENABLED=0 go build -o deeptalk ./cmd/server

# ---------- 阶段 2：运行 ----------
FROM alpine:latest

# ca-certificates：调用 HTTPS 的模型接口必须要有根证书，否则报 x509 错误
# tzdata：日志时间戳要按 Asia/Shanghai 显示
RUN apk --no-cache add ca-certificates tzdata wget && \
    ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

WORKDIR /app

COPY --from=builder /app/deeptalk .
COPY --from=builder /app/config ./config

# 用户上传的文档目录。运行时会被 volume 覆盖，这里先建好保证存在。
RUN mkdir -p uploads

EXPOSE 9090

CMD ["./deeptalk"]
