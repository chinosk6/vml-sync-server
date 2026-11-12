# vml-sync-server

游戏 Mod 的“模型同步”中转仓库。

Mod 让玩家在联机房间里使用自己的模型文件（`.vrca`）。房间里的其他人如果本机没有这个文件，就看不到它。
这个服务解决的就是这件事：**持有文件的玩家把它传上来，缺文件的玩家从这里下载。**

- 定位是朋友、小团体自用，不是公共服务。入口靠“地址 + 密钥”保护。
- 一个程序，自带管理页。**没有配置文件**：启动之后所有设置都在管理页里改，改完立刻生效，不用重启。
- 文件可以存在服务器本地磁盘，也可以存在 S3 兼容的对象存储（例如 Cloudflare R2）。
- 同一个文件不管谁传、传几次，云端只存一份；长时间没人下载的文件自动删除。
- 传上来的内容由服务器自己核对（大小、文件头、SHA-256），没通过核对的内容任何人都下载不到。

> **只同步你有权分享的模型。**

接口约定见 [docs/API.md](docs/API.md)。

## 目录

- [快速开始](#快速开始)
- [启动参数](#启动参数)
- [设置](#设置)
- [存储和传输方式怎么选](#存储和传输方式怎么选)
- [使用 Cloudflare R2](#使用-cloudflare-r2)
- [反向代理和 Cloudflare](#反向代理和-cloudflare)
- [用 Docker 运行](#用-docker-运行)
- [用 systemd 运行](#用-systemd-运行)
- [忘了管理员密码](#忘了管理员密码)
- [安全提示](#安全提示)
- [数据、备份和日常维护](#数据备份和日常维护)
- [从 0.1.0 升级](#从-010-升级)
- [开发](#开发)

## 快速开始

**1. 拿到程序。** 构建需要 Go 1.25 或更新的版本；构建出来的是一个不依赖任何运行库的可执行文件，拷到服务器上就能用。

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o vml-sync-server ./cmd/vml-sync-server
```

在 Windows 或 macOS 上给 Linux 服务器构建：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o vml-sync-server ./cmd/vml-sync-server
# ARM 服务器把 amd64 换成 arm64
```

**2. 直接运行。** 不需要准备任何文件。

```bash
./vml-sync-server
```

**3. 看日志里的初始密码。** 第一次启动时程序会生成管理员的初始密码，打印一次：

```
first start: generated a client key and an initial admin password
initial admin password: Kq7mX-4pRtW-9nHcB-e2YzD
it is also saved in /srv/vml/data/initial-admin-password.txt; log in to the admin page with it and change it there
vml-sync-server 0.2.0 listening on [::]:8080 (http), data directory /srv/vml/data, base_path="", storage=local, transfer=proxy
```

没看到也不要紧，它同时保存在数据目录的 `initial-admin-password.txt` 里（只有运行程序的用户能读）。

**4. 打开管理页改设置。** 地址是 `http://<服务器地址>:8080/admin/`，用初始密码登录，然后：

- **先把管理员密码改掉**（改掉之后初始密码文件会自动删除）；
- 按需要改别的设置：入口路径前缀、文件大小上限、存储位置……见下面的[设置](#设置)；
- 在页面上拿到要发给玩家的 `sync_server.json`（里面是地址和客户端密钥），发给朋友。朋友在游戏里按 R 打开轮盘 → 设置 → 模型同步 → 打开配置文件夹，把文件放进去
  （也就是 `%USERPROFILE%\AppData\LocalLow\Funcore Games\Worst Class Ever\Mods\Config\com.chinosk.vrc-model-loader\`；
  老版本放在游戏 `Mods\VrcModelLoader\` 里的也还认）。

程序第一次启动时已经随机生成了客户端密钥，默认设置（本地磁盘存储、单个文件 200 MB、72 小时没人下载就删除）开箱可用。

## 启动参数

启动时只有这几项，其余的都在管理页里。命令行参数优先于环境变量。

| 参数 | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `-listen` | `VML_LISTEN` | `:8080` | 监听地址。只让本机的反向代理访问时写 `127.0.0.1:8080` |
| `-data` | `VML_DATA_DIR` | `./data` | 数据目录（相对路径相对于启动时的当前目录）：设置、元数据、上传中的临时文件、本地存储的模型文件都在里面 |
| `-tls-cert`、`-tls-key` | `VML_TLS_CERT`、`VML_TLS_KEY` | 空 | 可选：让程序自己提供 HTTPS。两项要么都给要么都不给；不给就是明文 HTTP（由反向代理提供 HTTPS） |
| `-version` | | | 打印版本号后退出 |
| `-reset-admin-password` | | | 生成一个新的管理员密码并打印出来，然后退出。见[忘了管理员密码](#忘了管理员密码) |

`./vml-sync-server -h` 会列出这些参数。

## 设置

全部在管理页的“设置”里改，存在数据目录里，**改完立刻生效，不需要重启**。

| 设置 | 默认 | 说明 |
| --- | --- | --- |
| 多久没人下载就自动删除（`retention_hours`） | 72 | 0–87600 小时；0 = 不自动删除。从没被下载过的从上传时间算 |
| 入口路径前缀（`base_path`） | 空 | 例如 `/vml-3f9a1c`。所有接口和管理页都在它之下，之外的地址一律 404。只能用字母、数字和 `- _ . ~ /`，最长 64 |
| 客户端密钥（`auth_key`） | 首次启动时随机生成 | 填进 Mod 里的就是它。16–128 个可见 ASCII 字符。**改掉之后旧的立刻失效** |
| 单个文件上限（`max_file_mb`） | 200 | 1–5000 MB |
| 总容量上限（`max_total_mb`） | 0 | 0 = 不限。满了之后新的上传会被拒绝，直到有文件被删除 |
| 上传分片大小（`chunk_mb`） | 5 | 1–64 MB；传输方式是 `direct` 时不能小于 5。走反向代理时要小于代理允许的单次请求体上限 |
| 同时进行的上传上限（`max_active_uploads`） | 8 | 1–64 |
| 传输方式（`transfer`） | `proxy` | `proxy` = 文件内容经本服务中转；`direct` = 客户端凭临时地址直连对象存储（只有存储是 S3 时可用） |
| 临时地址有效期（`direct_url_ttl_seconds`） | 900 | 60–86400 秒 |
| 核对直连上传（`verify_direct_uploads`） | 开 | 直连上传完成后把内容读回来核对哈希。建议保持开启 |
| 信任转发头（`trust_forwarded_headers`） | 关 | 在受信任的反向代理后面时打开，见[下文](#反向代理和-cloudflare) |
| 存储（`storage`） | 本地磁盘 | 本地磁盘（数据目录的 `objects/` 下）或 S3 兼容对象存储 |

几件值得知道的事：

- **改限额和分片大小**对之后新开的上传生效；已经在传的那些按它们开始时的值传完。
- **改入口路径前缀**之后，页面会自动跳到新地址；旧地址从那一刻起就是 404。记得把新的 `sync_server.json` 发给玩家。
- **改客户端密钥**之后，所有玩家都要换上新的才能继续用。
- **换存储位置**（本地磁盘换成 S3、换一个桶、换前缀……）等于清空：现有的模型都留在旧位置，换过去之后拿不到。
  所以页面会让你确认；确认之后程序先验证新存储能用，然后删掉旧存储里的模型、作废进行中的上传，再切过去。
  **禁止名单保留。** 之后有人需要哪个模型，持有文件的玩家会自动再传上来。
- **只换 S3 的访问密钥**（位置没变）不算换存储：验证能用之后直接生效，模型都还在。
- S3 的机密访问密钥只能写、不能读：保存之后页面上不再显示它，留空就是保持原来的。
- 设置页里有“测试存储”，可以在保存之前先看看填的对不对。

## 存储和传输方式怎么选

| 组合 | 适合 | 说明 |
| --- | --- | --- |
| 本地磁盘 + `proxy` | 服务器磁盘和流量都够用 | 最简单，什么都不用另外配。默认就是这个 |
| S3 + `proxy` | 服务器磁盘小，但流量够用 | 文件存在对象存储里，上传下载的流量仍然经过你的服务器 |
| S3 + `direct` | 服务器流量贵、带宽小 | 客户端直接和对象存储收发文件，服务器只负责发放临时地址和核对 |

`direct` 模式下，客户端连不上对象存储时会自动退回 `proxy`（服务器始终同时支持两种方式），所以选 `direct` 不会让任何人用不了。

## 使用 Cloudflare R2

1. 在 Cloudflare 控制台的 **R2** 里创建一个桶（例如叫 `vml-models`）。位置随意。
2. 在 R2 的 **管理 R2 API 令牌（Manage R2 API Tokens）** 里创建一个令牌：
   - 权限选 **对象读和写（Object Read & Write）**；
   - 范围只选刚才那个桶。

   创建后记下 **访问密钥 ID（Access Key ID）** 和 **机密访问密钥（Secret Access Key）**。机密访问密钥只显示这一次。
3. 在同一个页面（或桶的设置页）找到 S3 API 的地址，形如 `https://<账户ID>.r2.cloudflarestorage.com`。
   控制台有时显示的是带桶名的地址（`…cloudflarestorage.com/vml-models`），**只要到主机名为止**。
4. 在管理页的设置里把存储改成 S3，填：

   | 项 | 填什么 |
   | --- | --- |
   | 服务地址（`endpoint`） | `https://<账户ID>.r2.cloudflarestorage.com` |
   | 区域（`region`） | `auto` |
   | 桶名（`bucket`） | `vml-models` |
   | 访问密钥 ID / 机密访问密钥 | 第 2 步记下的那两个 |
   | 对象名前缀（`prefix`） | `models/`（默认） |
   | 路径风格（`path_style`） | 开（默认） |

5. 点“测试存储”。通过之后保存，并确认“换存储会清空现有模型”。地址不通、密钥不对、桶不存在，测试和保存时都会直接告诉你是哪一种。

几点说明：

- 桶保持私有就行，**不需要**开公开访问、不需要绑自定义域名、不需要配 CORS（客户端不是浏览器）。
- 模型存成 `models/<模型 id>`。`direct` 上传时内容先落在 `models/_incoming/` 下，核对通过后才复制到正式位置，然后暂存对象被删掉。
  上传超时、被放弃，或者服务重启时，残留的暂存对象和没完成的分片上传会被清掉。
- **不要**给这个前缀配“过期自动删除对象”的生命周期规则，文件的去留由本服务管理。
  （对象万一被外部删掉了也不要紧：服务发现之后会清掉记录，持有文件的玩家会重新上传。）
- `direct` 模式下，客户端要能直接访问 `<账户ID>.r2.cloudflarestorage.com`。有的网络环境访问它不稳定，这时客户端会自动改走中转。
- 其他 S3 兼容的服务（MinIO、Backblaze B2、各家云的对象存储等）填法相同：地址、区域、桶名和密钥按那家的文档填；
  地址形如 `桶名.主机名` 的把“路径风格”关掉。需要对方支持分片上传、`ListParts`、`CopyObject` 和预签名地址。
- **存储出了问题怎么办**：如果对象存储用不了（比如密钥被吊销），程序照常运行、管理页照常能进，
  只是玩家的上传下载会暂时失败（接口返回 503）。把设置改对之后立刻恢复；如果只是对方临时故障，程序每 30 秒自己重试一次，恢复后不用你管。

## 反向代理和 Cloudflare

建议把服务放在提供 HTTPS 的反向代理后面：

- 启动时加 `-listen 127.0.0.1:8080`，只接受本机的代理转发；
- 在管理页的设置里打开“信任转发头”（`trust_forwarded_headers`）。

打开之后，程序取 `X-Forwarded-For` 的**最后一项**作为来源地址（登录失败限速按它算），
用 `X-Forwarded-Proto` 判断是不是 HTTPS（决定登录 Cookie 带不带 `Secure`）。
**程序直接暴露在公网上时不要打开它**，否则这两个请求头可以被任何人伪造。

### 为什么上传要分片：单次请求体的上限

反向代理和 CDN 都会限制单次请求体的大小，而模型文件常常有几十上百 MB：

- Cloudflare（橙色云朵 / Tunnel）：免费版和 Pro 是 **100 MB**，更高的套餐也只到 200–500 MB；
- nginx 的 `client_max_body_size` 默认只有 **1 MB**。

所以客户端把文件切成“上传分片大小”那么大的片，一片一个请求。你只需要保证：**代理允许的单次请求体 ≥ 分片大小**。
默认的 5 MB 在 Cloudflare 后面没有问题；nginx 需要把 `client_max_body_size` 调到比它大。

### nginx

```nginx
server {
    listen 443 ssl;
    server_name sync.example.com;
    # ssl_certificate / ssl_certificate_key ...

    location / {
        proxy_pass http://127.0.0.1:8080;      # 结尾不要带路径：地址要原样转给后端

        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        client_max_body_size    16m;    # 必须不小于“上传分片大小”
        proxy_request_buffering off;    # 上传的内容边收边转，不要先存到 nginx 的磁盘上
        proxy_buffering         off;    # 下载同理
        proxy_read_timeout      600s;   # “完成上传”要核对整个文件，可能要等一会儿
        proxy_send_timeout      600s;
    }
}
```

入口路径前缀是在管理页里改的，随时可能变，所以这里直接转发 `/` 最省事：前缀之外的地址程序自己会回 404。

### Caddy

```caddyfile
sync.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Caddy 会自动申请证书、自动带上 `X-Forwarded-For` / `X-Forwarded-Proto`，默认也不限制请求体大小。

### 套 Cloudflare（橙色云朵或 Tunnel）

- **请求体上限**见上，默认设置没有问题。
- **响应超时**：Cloudflare 等源站响应最多约 100 秒，超时会返回 524。绝大多数请求都远远用不了这么久；
  可能接近的只有大文件的“完成上传”（要核对整个文件；用对象存储时还要把文件传过去或读回来），以及换存储位置时的那一次保存。
  超时不会弄坏任何东西：服务器会把这一步做完，再请求一次拿到的就是结果。
- **下载走的也是你的服务器**（`proxy` 模式）。流量大的话可以考虑 S3 + `direct`，文件内容就不经过你的服务器和 Cloudflare 代理了。
- 如果 Cloudflare 和程序之间还有一层 nginx，nginx 看到的来源是 Cloudflare 的节点地址，登录限速会按节点算（更粗，但仍然有效）。
  想按真实访客算，可以在 nginx 里用 `real_ip` 模块还原来源地址。

## 用 Docker 运行

仓库里带了 [Dockerfile](Dockerfile)（多阶段构建，最终镜像只有程序本身和 CA 证书，以非 root 用户运行）。
容器里监听 `:8080`，数据目录是 `/data`（由环境变量 `VML_LISTEN`、`VML_DATA_DIR` 决定，一般不用改）。

```bash
docker build -t vml-sync-server .
# 国内构建可以加：--build-arg GOPROXY=https://goproxy.cn,direct

docker run -d --name vml-sync-server --restart unless-stopped \
  -p 8080:8080 \
  -v vml-data:/data \
  vml-sync-server

docker logs vml-sync-server        # 第一次启动：在这里看管理员的初始密码
```

或者用 Compose：

```yaml
services:
  vml-sync-server:
    build: .
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"     # 只给本机的反向代理用；要直接对外就写 "8080:8080"
    volumes:
      - vml-data:/data
    stop_grace_period: 30s

volumes:
  vml-data:
```

想把数据放在宿主机的某个目录里（而不是 Docker 的卷），把 `vml-data:/data` 换成 `./data:/data`，
并先让容器里的用户（uid 10001）能写它：`mkdir -p data && sudo chown 10001:10001 data`。

初始密码也可以这样看：`docker exec vml-sync-server cat /data/initial-admin-password.txt`。

## 用 systemd 运行

示例单元文件在 [deploy/vml-sync-server.service](deploy/vml-sync-server.service)。

```bash
sudo useradd --system --home /opt/vml-sync-server --shell /usr/sbin/nologin vml
sudo mkdir -p /opt/vml-sync-server/data
sudo cp vml-sync-server /opt/vml-sync-server/
sudo chown -R vml:vml /opt/vml-sync-server

sudo cp deploy/vml-sync-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now vml-sync-server

journalctl -u vml-sync-server -n 20                              # 第一次启动：在这里看初始密码
sudo cat /opt/vml-sync-server/data/initial-admin-password.txt    # 或者看这个文件
```

单元文件里默认监听 `127.0.0.1:8080`（给本机的反向代理用）。想直接对外，把里面的 `-listen` 改成 `:8080`。

程序收到 `SIGTERM`（`systemctl stop`）后会优雅退出：不再接新请求，给进行中的请求最多 20 秒收尾，
然后作废没传完的上传并清掉它们的临时数据。

## 忘了管理员密码

先把服务停掉（数据文件同一时间只能被一个进程打开），然后：

```bash
./vml-sync-server -data <数据目录> -reset-admin-password
```

它会生成一个新密码、打印出来，并写进数据目录的 `initial-admin-password.txt`。再把服务启动起来，用新密码登录后在管理页里改成你自己的。
别的设置和数据都不受影响。

- systemd：`sudo systemctl stop vml-sync-server`，然后
  `sudo -u vml /opt/vml-sync-server/vml-sync-server -data /opt/vml-sync-server/data -reset-admin-password`，再 `sudo systemctl start vml-sync-server`。
- Docker：`docker stop vml-sync-server`，然后
  `docker run --rm -v vml-data:/data vml-sync-server -reset-admin-password`，再 `docker start vml-sync-server`。

## 安全提示

- **第一件事是改掉初始密码。** 还在用初始密码时，管理页会一直提醒，程序每次启动也会在日志里提醒一句。
- **两个密钥是两回事。** 客户端密钥是发给朋友的，知道它的人都能上传和下载；管理员密码只有你自己知道，不要发给别人。
- **建议用 HTTPS。** 明文 HTTP 下，密钥、管理密码和文件内容在网络上都是可以被旁人看到的。
  用反向代理（Caddy 最省事）、Cloudflare，或者启动参数里的 `-tls-cert` / `-tls-key` 都可以。
- **入口路径前缀只是让入口不容易被扫到**，真正的保护是密钥。客户端密钥保持默认生成的长随机串就很好。
- **密钥泄露了就换。** 在管理页里换一个客户端密钥，旧的立刻失效，再把新的发给朋友。
- **数据目录要保护好。** 里面有客户端密钥、对象存储的访问密钥（明文）和管理员密码的哈希（加盐的慢哈希，不是明文）。
  不要把数据目录提交到代码仓库或者放到别人能读的地方（仓库的 `.gitignore` 已经排除了 `data/`）。
- **服务器只核对“是不是资源包、内容和 id 对不对得上”**，不判断模型里面是什么。只把地址和密钥交给你信任的人。
- **“信任转发头”只在反向代理后面打开**，原因见[上文](#反向代理和-cloudflare)。
- 管理员密码连续输错会被暂时挡住（同一来源 5 分钟内 5 次）。登录会话 12 小时过期，服务重启后要重新登录。
- 日志里不会出现密钥、密码和临时地址——除了第一次启动时打印一次的初始密码。
- 只同步你有权分享的模型。发现不该出现的模型，在管理页“删除并禁止”。

## 数据、备份和日常维护

数据目录里有：

| 路径 | 内容 |
| --- | --- |
| `meta.db` | 全部设置、管理员密码的哈希、模型记录（文件名、上传者、下载次数等）、禁止名单 |
| `objects/` | 本地存储的模型文件（文件名就是模型 id）。用对象存储时是空的 |
| `uploads/` | 上传中的临时文件，传完或超时后自动清掉 |
| `initial-admin-password.txt` | 管理员的初始密码。改掉密码之后就没有这个文件了 |

- **备份**：停掉服务，复制整个数据目录（用对象存储的话只需要 `meta.db`）。
  模型文件本身丢了也不要紧，有人需要时持有文件的玩家会再传一遍；值得备份的是设置和禁止名单。
- **对账**：程序每次启动（以及换存储之后）都会让记录和存储一致——有记录没文件的删掉记录；有文件没记录的，
  核对内容确实和文件名（id）对得上之后补一条记录（文件名显示为 `(unknown)`）；对不上的不登记、也不删除，只在日志里提醒。
- **后台任务**：每 10 分钟清理一次过期的模型；上传会话空闲超时（中转 120 秒没有数据流入、直连 900 秒没有任何调用）后作废。
- **同一个数据目录只能有一个实例在跑。**
- 普通的删除只是腾空间：之后还有人需要的话，持有文件的玩家会再传上来。想让一个模型彻底不再出现，用“删除并禁止”。
- 日志打到标准输出，每行带时间；访问日志的格式是 `来源地址 方法 "路径" 状态码 响应字节数 耗时`。

## 从 0.1.0 升级

0.1.0 用配置文件（`config.yaml`）；现在没有配置文件了。

- 启动命令从 `./vml-sync-server -config config.yaml` 改成 `./vml-sync-server -listen <监听地址> -data <数据目录>`。
  还带着 `-config` 启动的话，程序会说明情况然后退出。
- 原来的数据目录直接接着用：模型、禁止名单、“多久没人下载就自动删除”都原样保留。
- 配置文件里的其他内容**不会**被读取。第一次用新版本启动时，程序会生成新的客户端密钥和管理员初始密码（看日志），其余设置是默认值。
  登录管理页之后，把需要的改回去——尤其是：
  - **客户端密钥**：想让玩家手里的 `sync_server.json` 继续能用，就把它改回原来的 `auth_key`（要求 16–128 个可见 ASCII 字符）；
  - **入口路径前缀**（原来的 `base_path`）；
  - 用对象存储的话，把存储改回原来的桶（这一步会要求确认；填的是原来的位置时，里面的模型在保存之后会被重新登记，文件名显示为 `(unknown)`）。
- 配置里的 `storage.local.dir` 没有了：本地存储固定在数据目录的 `objects/` 下。

## 开发

```bash
go build ./...
go vet ./...
go test ./...
```

测试不需要任何外部服务：S3 相关的测试用的是进程内的假 S3 服务，并且会校验每个请求的签名。

仓库布局：

```
cmd/vml-sync-server/     程序入口：启动参数、首次启动、-reset-admin-password
internal/settings/       设置：默认值、取值范围、局部更新
internal/passwd/         管理员密码：加盐的慢哈希、初始密码的生成
internal/meta/           元数据（bbolt，纯 Go 的单文件嵌入式存储）
internal/storage/        存储抽象，本地磁盘和 S3 两个实现
internal/s3/             精简的 S3 客户端（SigV4 签名、分片上传、预签名地址）
internal/app/            核心逻辑：设置热生效、上传会话、内容核对、禁止名单、过期清理、对账
internal/httpapi/        HTTP 路由、鉴权、客户端接口和管理接口
internal/s3test/         只给测试用的假 S3 服务
web/embed.go             把 web/admin 内嵌进程序
web/admin/               管理页的静态文件
web/devmock/             只给管理页开发用的假服务（不进正式程序）
docs/API.md              接口约定
Dockerfile, deploy/      部署用的文件
```

运行时只依赖一个纯 Go 的库：`go.etcd.io/bbolt`（元数据）。不需要 CGO。

改管理页时注意：服务端给管理页的响应带了内容安全策略（CSP），脚本、样式、图片和接口请求都只能来自本站，
并且**不允许内联脚本**（`<script>` 里直接写代码、`onclick=` 这类属性都不会执行）；内联样式是允许的。

发布构建时可以写入版本号：

```bash
go build -ldflags "-s -w -X vml-sync-server/internal/version.Version=1.2.3" ./cmd/vml-sync-server
```
