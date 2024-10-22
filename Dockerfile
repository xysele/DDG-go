# 使用官方的 Golang 镜像进行构建
FROM golang:1.22 AS builder

# 在容器中设置工作目录
WORKDIR /app

# 将 go.mod 和 go.sum 复制到工作目录
COPY go.mod go.sum ./

# 下载所有依赖（如果 go.mod 没有变化则会使用缓存）
RUN go mod download

# 将源代码复制到工作目录
COPY . .

# 静态编译 Go 应用程序，确保兼容 Alpine
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ddg-chat-go .

# 使用更小的 Alpine 镜像以减小最终镜像体积
FROM alpine:latest

# 在容器中设置工作目录
WORKDIR /root/

# 从构建阶段复制编译好的二进制文件
COPY --from=builder /app/ddg-chat-go .

# 设置环境变量
ENV API_PREFIX="/"         
ENV MAX_RETRY_COUNT="3"  
ENV RETRY_DELAY="5000"    
ENV PORT="8787"

# 暴露应用运行的端口
EXPOSE 8787

# 确保二进制文件可执行
RUN chmod +x ddg-chat-go

# 启动二进制文件
CMD ["./ddg-chat-go"]
