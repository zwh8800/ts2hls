# 基础镜像
FROM golang:buster AS build

# 设置工作目录
WORKDIR /app

# 复制项目文件到容器中
COPY . .

# 构建应用程序
RUN go build -o ts2hls

# 运行镜像
FROM debian:buster-slim
WORKDIR /app
COPY --from=build /app/ts2hls .
ENTRYPOINT ["tini", "--"]
CMD ["./ts2hls"]
