# vml-sync-server 的容器镜像（多阶段构建）。
#
#   docker build -t vml-sync-server .
#   docker run -d --name vml-sync-server -p 8080:8080 -v vml-data:/data vml-sync-server
#   docker logs vml-sync-server        # 第一次启动：在这里看管理员的初始密码
#
# 没有配置文件：所有设置都在管理页（http://<主机>:8080/admin/）里改，存在 /data 里。

FROM golang:1.25-alpine AS build
# 国内构建可以传 --build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0 GOFLAGS=-trimpath
WORKDIR /src
# 先只复制模块文件，依赖下载这一层就能被缓存。
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags "-s -w" -o /out/vml-sync-server ./cmd/vml-sync-server

FROM alpine:3.22
# CA 证书：访问对象存储的 HTTPS 接口要用。
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 vml \
    && mkdir -p /data \
    && chown vml:vml /data
COPY --from=build /out/vml-sync-server /usr/local/bin/vml-sync-server
USER vml
# 启动参数通过环境变量给：监听地址和数据目录。
ENV VML_LISTEN=:8080 VML_DATA_DIR=/data
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/vml-sync-server"]
