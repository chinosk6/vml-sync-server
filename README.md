# vml-sync-server

[VRC Model Loader](https://steamcommunity.com/sharedfiles/filedetails/?id=3837926039) 的模型同步中转仓库，用于将本地使用的模型同步给远程玩家。



- 支持使用服务器储存和 S3 兼容的对象存储。
- 同步后的文件不会加密，请不要同步自己的私人模型或者无权分发的模型。



接口约定见 [docs/API.md](docs/API.md)。



## 目录

- [快速开始](#快速开始)
- [启动参数](#启动参数)
- [设置](#设置)
- [存储与传输方式](#存储与传输方式)
- [Cloudflare R2](#cloudflare-r2)
- [反向代理](#反向代理)
- [Docker](#docker)
- [systemd](#systemd)
- [重置管理员密码](#重置管理员密码)
- [安全](#安全)
- [数据与维护](#数据与维护)
- [开发](#开发)

## 快速开始

构建需要 Go 1.25 及以上，产物为无外部依赖的单个可执行文件。

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o vml-sync-server ./cmd/vml-sync-server
```

交叉编译 Linux 版本（ARM 服务器将 `amd64` 换成 `arm64`）：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o vml-sync-server ./cmd/vml-sync-server
```

运行：

```bash
./vml-sync-server
```

首次启动会生成客户端密钥和管理员初始密码。初始密码打印在日志中，同时保存在数据目录的 `initial-admin-password.txt`（权限 600）：

```
first start: generated a client key and an initial admin password
initial admin password: Kq7mX-4pRtW-9nHcB-e2YzD
it is also saved in /srv/vml/data/initial-admin-password.txt; log in to the admin page with it and change it there
vml-sync-server 0.2.0 listening on [::]:8080 (http), data directory /srv/vml/data, base_path="", storage=local, transfer=proxy
```

访问 `http://<服务器地址>:8080/admin/` 登录管理页：

1. 修改管理员密码。修改后初始密码文件会被删除。
2. 按需修改其他[设置](#设置)。
3. 下载 `sync_server.json`（包含服务地址和客户端密钥）并分发给玩家。玩家在游戏中按 R 打开轮盘 → 设置 → 模型同步 → 打开配置文件夹，将文件放入。
   目录为 `%USERPROFILE%\AppData\LocalLow\Funcore Games\Worst Class Ever\Mods\Config\com.chinosk.vrc-model-loader\`，旧版本的 `Mods\VrcModelLoader\` 仍可识别。

默认设置：本地磁盘存储，单文件上限 200 MB，72 小时无下载自动删除。

## 启动参数

其余设置均在管理页中修改。命令行参数优先于环境变量。

| 参数 | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `-listen` | `VML_LISTEN` | `:8080` | 监听地址。仅供本机反向代理访问时使用 `127.0.0.1:8080` |
| `-data` | `VML_DATA_DIR` | `./data` | 数据目录，存放设置、元数据、上传临时文件和本地存储的模型 |
| `-tls-cert`、`-tls-key` | `VML_TLS_CERT`、`VML_TLS_KEY` | 空 | 启用 HTTPS 的证书和私钥，需同时指定 |
| `-version` | | | 打印版本号并退出 |
| `-reset-admin-password` | | | 重置管理员密码并退出，见[重置管理员密码](#重置管理员密码) |

## 设置

在管理页“设置”中修改，保存在数据目录，即时生效，无需重启。

| 设置 | 默认 | 说明 |
| --- | --- | --- |
| `retention_hours` | 72 | 无下载自动删除的时长，0–87600 小时，0 为不删除。未被下载过的从上传时间计算 |
| `base_path` | 空 | 入口路径前缀，如 `/vml-3f9a1c`。前缀外的地址均返回 404。允许字母、数字和 `- _ . ~ /`，最长 64 |
| `auth_key` | 首次启动随机生成 | 客户端密钥，16–128 个可见 ASCII 字符。修改后旧密钥立即失效 |
| `max_file_mb` | 200 | 单文件上限，1–5000 MB |
| `max_total_mb` | 0 | 总容量上限，0 为不限。达到上限后拒绝新上传 |
| `chunk_mb` | 5 | 上传分片大小，1–64 MB；`direct` 模式下不小于 5。需小于反向代理的请求体上限 |
| `max_active_uploads` | 8 | 同时进行的上传数，1–64 |
| `transfer` | `proxy` | `proxy`：文件经本服务中转；`direct`：客户端通过预签名地址直连对象存储（仅 S3） |
| `direct_url_ttl_seconds` | 900 | 预签名地址有效期，60–86400 秒 |
| `verify_direct_uploads` | 开 | 直连上传完成后读回内容校验哈希 |
| `trust_forwarded_headers` | 关 | 信任 `X-Forwarded-*` 请求头，见[反向代理](#反向代理) |
| `storage` | 本地磁盘 | 本地磁盘（数据目录 `objects/`）或 S3 兼容对象存储 |

- 限额和分片大小的修改仅对新上传生效。
- 修改 `base_path` 后旧地址立即失效，需重新分发 `sync_server.json`。
- 修改 `auth_key` 后需重新分发 `sync_server.json`。
- 更换存储位置（类型、桶、前缀等）需确认。确认后验证新存储可用，删除旧存储中的模型并取消进行中的上传，再切换。禁止名单保留。
- 仅更换 S3 访问密钥不视为更换存储位置，验证通过后直接生效。
- S3 机密访问密钥保存后不再显示，留空表示不修改。
- 可使用“测试存储”在保存前验证存储配置。

## 存储与传输方式

| 组合 | 适用场景 |
| --- | --- |
| 本地磁盘 + `proxy` | 默认。服务器磁盘和流量充足 |
| S3 + `proxy` | 服务器磁盘不足，流量充足 |
| S3 + `direct` | 服务器带宽或流量受限。文件不经过服务器 |

`direct` 模式下，客户端无法连接对象存储时会自动回退到 `proxy`。

## Cloudflare R2

1. 在 R2 中创建桶，如 `vml-models`。
2. 在“管理 R2 API 令牌”中创建令牌，权限为“对象读和写”，范围限定为该桶。记录访问密钥 ID 和机密访问密钥。
3. 获取 S3 API 地址 `https://<账户ID>.r2.cloudflarestorage.com`，不包含桶名。
4. 在管理页中将存储改为 S3：

   | 项 | 值 |
   | --- | --- |
   | `endpoint` | `https://<账户ID>.r2.cloudflarestorage.com` |
   | `region` | `auto` |
   | `bucket` | `vml-models` |
   | `access_key_id` / `secret_access_key` | 第 2 步的令牌 |
   | `prefix` | `models/`（默认） |
   | `path_style` | 开（默认） |

5. 测试存储，通过后保存并确认。

说明：

- 桶保持私有即可，无需公开访问、自定义域名或 CORS。
- 模型保存为 `<prefix><模型 id>`。`direct` 上传先写入 `<prefix>_incoming/`，校验通过后复制到正式位置。残留的暂存对象和未完成的分片上传会被自动清理。
- 不要对该前缀配置生命周期过期规则。
- 其他 S3 兼容服务（MinIO、Backblaze B2 等）配置方式相同，虚拟主机风格地址需关闭 `path_style`。服务需支持分片上传、`ListParts`、`CopyObject` 和预签名地址。
- 对象存储不可用时，上传和下载接口返回 503，管理页正常可用。服务每 30 秒重试一次，恢复后自动可用。

## 反向代理

建议在反向代理后运行：

- 使用 `-listen 127.0.0.1:8080`；
- 开启 `trust_forwarded_headers`。

开启后，`X-Forwarded-For` 的最后一项作为来源地址（用于登录限速），`X-Forwarded-Proto` 用于判断 HTTPS（决定 Cookie 的 `Secure` 属性）。直接暴露在公网时不要开启。

反向代理的请求体上限需大于 `chunk_mb`。nginx 默认 `client_max_body_size` 为 1 MB；Cloudflare 免费版和 Pro 为 100 MB。

### nginx

```nginx
server {
    listen 443 ssl;
    server_name sync.example.com;
    # ssl_certificate / ssl_certificate_key ...

    location / {
        proxy_pass http://127.0.0.1:8080;      # 不带路径，原样转发

        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        client_max_body_size    16m;    # 不小于 chunk_mb
        proxy_request_buffering off;
        proxy_buffering         off;
        proxy_read_timeout      600s;   # 完成上传时需校验整个文件
        proxy_send_timeout      600s;
    }
}
```

`base_path` 可在管理页随时修改，因此直接转发 `/`。

### Caddy

```caddyfile
sync.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

### Cloudflare

- 源站响应超时约 100 秒，超时返回 524。大文件的“完成上传”和更换存储可能接近该时长；超时后服务端仍会完成操作，重试即可得到结果。
- `proxy` 模式下下载流量经过服务器。流量较大时可使用 S3 + `direct`。
- Cloudflare 与本服务之间有 nginx 时，可使用 nginx `real_ip` 模块还原访客地址。

## Docker

镜像使用非 root 用户（uid 10001）运行，监听 `:8080`，数据目录为 `/data`。

```bash
docker build -t vml-sync-server .
# 可选：--build-arg GOPROXY=https://goproxy.cn,direct

docker run -d --name vml-sync-server --restart unless-stopped \
  -p 8080:8080 \
  -v vml-data:/data \
  vml-sync-server

docker logs vml-sync-server        # 查看初始密码
```

Compose：

```yaml
services:
  vml-sync-server:
    build: .
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - vml-data:/data
    stop_grace_period: 30s

volumes:
  vml-data:
```

使用宿主机目录时将 `vml-data:/data` 改为 `./data:/data`，并执行 `mkdir -p data && sudo chown 10001:10001 data`。

## systemd

单元文件见 [deploy/vml-sync-server.service](deploy/vml-sync-server.service)，默认监听 `127.0.0.1:8080`。

```bash
sudo useradd --system --home /opt/vml-sync-server --shell /usr/sbin/nologin vml
sudo mkdir -p /opt/vml-sync-server/data
sudo cp vml-sync-server /opt/vml-sync-server/
sudo chown -R vml:vml /opt/vml-sync-server

sudo cp deploy/vml-sync-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now vml-sync-server

journalctl -u vml-sync-server -n 20                              # 查看初始密码
sudo cat /opt/vml-sync-server/data/initial-admin-password.txt
```

收到 `SIGTERM` 后停止接收新请求，等待进行中的请求最多 20 秒，然后取消未完成的上传并清理临时数据。

## 重置管理员密码

需先停止服务：

```bash
./vml-sync-server -data <数据目录> -reset-admin-password
```

新密码会打印出来并写入 `initial-admin-password.txt`，其他设置和数据不受影响。

- systemd：`sudo systemctl stop vml-sync-server` 后执行
  `sudo -u vml /opt/vml-sync-server/vml-sync-server -data /opt/vml-sync-server/data -reset-admin-password`，再启动服务。
- Docker：`docker stop vml-sync-server` 后执行
  `docker run --rm -v vml-data:/data vml-sync-server -reset-admin-password`，再启动容器。

## 安全

- 首次登录后修改初始密码。
- 客户端密钥用于上传和下载，分发给玩家；管理员密码不要分发。
- 建议使用 HTTPS（反向代理、Cloudflare 或 `-tls-cert` / `-tls-key`）。
- `base_path` 仅用于隐藏入口，访问控制依赖密钥。
- 密钥泄露时在管理页更换 `auth_key`。
- 数据目录包含客户端密钥、S3 访问密钥（明文）和管理员密码哈希（PBKDF2），注意权限，不要提交到代码仓库。
- 服务端仅校验文件格式、大小和哈希，不检查模型内容。
- 同一来源 5 分钟内登录失败 5 次会被暂时限制。登录会话 12 小时过期，重启服务后失效。
- 日志不记录密钥、密码和预签名地址（首次启动打印的初始密码除外）。
- 管理页的“删除并禁止”可阻止模型再次上传。

## 数据与维护

| 路径 | 内容 |
| --- | --- |
| `meta.db` | 设置、管理员密码哈希、模型记录、禁止名单 |
| `objects/` | 本地存储的模型文件，文件名为模型 id |
| `uploads/` | 上传临时文件 |
| `initial-admin-password.txt` | 初始密码，修改密码后删除 |

- 备份：停止服务后复制数据目录。使用对象存储时只需备份 `meta.db`。
- 对账：启动和更换存储后，删除无对应文件的记录；对无记录的文件，校验哈希与文件名一致后补登记（名称为 `(unknown)`），不一致的仅记录日志。
- 每 10 分钟清理过期模型。上传会话空闲超时后取消（`proxy` 120 秒无数据，`direct` 900 秒无请求）。
- 同一数据目录只能运行一个实例。
- 访问日志格式：`来源地址 方法 "路径" 状态码 响应字节数 耗时`。

## 开发

```bash
go build ./...
go vet ./...
go test ./...
```

测试不依赖外部服务，S3 测试使用进程内的模拟服务并校验请求签名。

```
cmd/vml-sync-server/     程序入口
internal/settings/       设置定义与校验
internal/passwd/         管理员密码哈希
internal/meta/           元数据（bbolt）
internal/storage/        存储接口，本地磁盘和 S3 实现
internal/s3/             S3 客户端（SigV4、分片上传、预签名）
internal/app/            核心逻辑：设置、上传、校验、清理、对账
internal/httpapi/        HTTP 路由、鉴权、客户端与管理接口
internal/s3test/         测试用 S3 模拟服务
web/admin/               管理页静态文件，通过 web/embed.go 内嵌
web/devmock/             管理页开发用模拟服务
docs/API.md              接口约定
```

唯一的运行时依赖为 `go.etcd.io/bbolt`，无需 CGO。

管理页受 CSP 限制：资源和请求仅限同源，不允许内联脚本（包括 `onclick=` 等属性），允许内联样式。

构建时指定版本号：

```bash
go build -ldflags "-s -w -X vml-sync-server/internal/version.Version=1.2.3" ./cmd/vml-sync-server
```
