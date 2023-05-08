# 第一阶段：构建应用程序
FROM golang:alpine AS builder

# 设置工作目录为 /app
WORKDIR /app

# 复制当前目录中的文件到容器中
COPY . .

# 在构建阶段编译二进制文件
RUN CGO_ENABLED=1 go build -o myapp .

# 第二阶段：执行环境
FROM alpine:latest

# 定义工作目录
WORKDIR /root/

# 从第一阶段复制二进制文件
COPY --from=builder /app/myapp .

# 指定容器启动时要运行的命令
CMD ["./myapp"]
