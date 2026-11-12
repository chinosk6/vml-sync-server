# vml-sync-server 接口约定（v1）

这份文档是服务端、管理页、游戏内 Mod 三方共同遵守的约定。三方各自按它实现，互不依赖对方的代码。
改动约定要先改这份文档。

## 0. 这个服务是做什么的

游戏 Mod 让玩家在联机房间里使用自己的模型文件（`.vrca`）。房间里的其他人如果本机没有这个文件，就看不到它。
这个服务充当中转仓库：持有文件的玩家把它传上来，缺文件的玩家从这里下载。

- 定位是朋友、小团体自用，不是公共服务。入口靠“地址 + 密钥”保护。
- 一个 Go 程序，自带管理页（不分前后端）。**没有配置文件**：除了监听地址和数据目录这两个启动参数，所有设置都在管理页里改，存在数据目录里，改完立刻生效。
- 文件存在 S3 兼容的对象存储（例如 Cloudflare R2）或服务器本地磁盘。
- 长时间没人下载的文件自动删除。

## 1. 通用规则

- 所有地址都在 `{base}` 之下：`{base} = 协议://主机[:端口]{base_path}`。`base_path` 是一项设置（在管理页里改），可以为空；
  用来给入口加一段不容易被扫到的路径。`{base}` 之外的任何路径一律 404，不暴露任何信息。
- 请求体和响应体都是 JSON（UTF-8），文件内容除外。
- 时间一律是 Unix 秒（整数），0 表示“没有”。大小一律是字节数。
- **模型 id**：文件内容 SHA-256 的前 16 个字节，小写十六进制，共 32 个字符，必须匹配 `^[0-9a-f]{32}$`。
  id 由内容决定，所以同一个文件不管谁传、传几次都是同一个 id。服务器必须自己计算内容的 SHA-256 来核对，不信任客户端报的 id。
- **业务结果**用 `200` 加响应体里的 `state` 字段表达。客户端遇到不认识的 `state` 一律按“失败，稍后再试”处理，
  所以以后可以加新的 `state`。
- **出错**用非 2xx 状态码，响应体 `{"error":"<代码>","message":"<给人看的说明>"}`：
  `400` 请求格式不对（`bad_request`、`bad_id`、`bad_part`）；`401` 鉴权失败（`unauthorized`）；`404`（`not_found`、`upload_gone`）；
  `409`（`wrong_mode`）；`413` 请求体超限（`too_large`）；`429`（`too_many_attempts`）；`5xx` 服务器内部错误。
- 响应一律带 `Cache-Control: no-store`。

### 1.1 客户端鉴权

`/api/v1/` 下除管理接口外的所有接口：请求头 `Authorization: Bearer <auth_key>`。`auth_key` 是一项设置（首次启动时随机生成，在管理页里查看和更换），比较时用常数时间比较。
不带或不对一律 `401`。

### 1.2 管理员鉴权

管理接口（`/api/v1/admin/`）用登录会话：

- `POST /api/v1/admin/login`，请求体 `{"password":"…"}`。管理员密码只以加盐的慢哈希形式存在数据目录里（首次启动时生成初始密码，见第 6 节；在管理页里修改，见 3.7）。
  成功：`200 {"ok":true}`，并下发 Cookie `vml_admin=<随机令牌>; HttpOnly; SameSite=Strict; Path={base_path}/; Max-Age=43200`
  （请求是 HTTPS，或者受信任的反向代理报告是 HTTPS 时加 `Secure`）。
  失败：`401 {"error":"bad_password"}`。同一来源 5 分钟内失败满 5 次：`429 {"error":"too_many_attempts","retry_after":<秒>}`。
- `POST /api/v1/admin/logout` → `200 {"ok":true}`，会话作废。
- `GET /api/v1/admin/session` → `200 {"authenticated":true|false}`（这个接口永远不返回 401）。
- 其余管理接口没有有效会话时一律 `401 {"error":"unauthorized"}`。
- 会改动数据的管理请求（POST / PUT / DELETE，登录除外）必须带请求头 `X-VML-Admin: 1`，否则 `400`。
- 会话存在内存里，12 小时过期，服务重启后要重新登录。

## 2. 客户端接口

### 2.1 探测

```
GET {base}/api/v1/ping
200 {"ok":true,"name":"vml-sync-server","version":"0.1.0","api":1,
     "transfer":"proxy","max_file_bytes":209715200,"chunk_bytes":5242880}
```

`transfer` 是服务器默认的传输方式：`proxy`（文件内容经本服务中转）或 `direct`（客户端凭临时地址直连对象存储）。

### 2.2 查询模型状态

```
GET {base}/api/v1/models/{id}
200 {"id":"…","state":"ready","size":123}
200 {"id":"…","state":"uploading","size":123,"received":45}
200 {"id":"…","state":"missing"}
200 {"id":"…","state":"blocked"}
```

- `uploading`：有人正在传。`received` 是已经收到的字节数，只用于显示进度：
  `proxy` 模式下是已收到的分片的总大小；`direct` 模式下按“已经签发到第几片”估算（请求第 n 片的地址即视为前 n-1 片已完成）。
- `blocked`：管理员禁止了这个模型，不接受上传，也不提供下载。
- 这个接口不算一次下载。

### 2.3 开始上传

```
POST {base}/api/v1/models/{id}/upload
{"size":123,"name":"文件名.vrca","uploader":"上传者昵称","mode":"proxy"}
```

- `size` 必填，大于 0。`name`（最长 200 个字符）和 `uploader`（最长 64 个字符）只用于管理页显示，服务器要截断超长的、去掉控制字符。
- `mode` 可选：客户端要求这一次走 `proxy`（直连对象存储失败后的退路）。服务器默认 `direct` 时也必须接受 `proxy`；
  服务器默认 `proxy` 时忽略这个字段。

响应：

```
200 {"state":"ready"}                                   云端已经有了，不用传
200 {"state":"blocked"}
200 {"state":"too_large","max_file_bytes":209715200}
200 {"state":"storage_full"}                            配置了总容量上限并且已满
200 {"state":"busy","retry_after":10,"size":123,"received":45}   别人正在传同一个 id
200 {"state":"upload","upload_id":"…","mode":"proxy","chunk_bytes":5242880,"part_count":3}
```

- `upload`：开了一个上传会话。文件按 `chunk_bytes` 切成 `part_count = ceil(size / chunk_bytes)` 片，
  第 n 片（从 1 开始）是字节 `[(n-1)*chunk_bytes, min(n*chunk_bytes, size))`。
- 同一个 id 同时只允许一个上传会话。会话空闲超时后作废、清掉临时数据，id 回到 `missing`：
  `proxy` 模式按“有没有字节在流入”算，默认 120 秒；`direct` 模式按最后一次接口调用算，默认 900 秒。
- 同时存在的上传会话总数有上限（设置项 `max_active_uploads`，默认 8），超了返回 `busy`。

### 2.4 上传分片

`proxy` 模式：

```
PUT {base}/api/v1/uploads/{upload_id}/parts/{n}
（请求体是这一片的原始字节，Content-Length 必须等于这一片应有的长度）
200 {"received":10485760}
```

`direct` 模式：

```
POST {base}/api/v1/uploads/{upload_id}/parts/{n}/url
200 {"url":"https://…","expires_in":900}
```

然后客户端对这个 `url` 发 `PUT`，请求体是这一片的原始字节，**不带** `Authorization`，也不带任何额外请求头；任何 2xx 都算成功。
服务器签发的地址必须只要求“方法是 PUT”，不能要求特定的请求头。

- 分片可以重传，重传覆盖之前的同一片（幂等）。分片不要求按顺序，但客户端实际上是按顺序、一次一片地传。
- 会话不存在或已超时：`404 {"error":"upload_gone"}`。片号超范围、长度不对：`400 {"error":"bad_part"}`。
  在 `direct` 会话上调 `proxy` 的分片接口或反过来：`409 {"error":"wrong_mode"}`。

### 2.5 完成上传

```
POST {base}/api/v1/uploads/{upload_id}/complete
200 {"state":"ready","id":"…"}
200 {"state":"incomplete","missing":[2,3]}
200 {"state":"rejected","reason":"size_mismatch"}
```

- 服务器在这一步核对：所有分片齐全；总大小等于声明的 `size`；内容以 `UnityFS` 这 7 个字节开头；
  内容 SHA-256 的前 16 字节等于 id。全部通过才登记为 `ready`。
- `incomplete`：缺片。会话保留，客户端补传 `missing` 里的片之后再调一次。
- `rejected`：`reason` 是 `size_mismatch`、`hash_mismatch`、`not_a_bundle` 之一。会话作废，数据删掉。
- **没有通过核对的内容永远不能被下载到。**
- `direct` 模式下服务器要从对象存储把内容读回来算哈希，这个请求可能要等几十秒；客户端的超时按 10 分钟设。
- 可选的礼貌性接口：`POST {base}/api/v1/uploads/{upload_id}/abort` → `200 {"ok":true}`（会话不存在也返回 200）。

### 2.6 下载

```
POST {base}/api/v1/models/{id}/download
（请求体可以为空，或 {"mode":"proxy"} 要求这一次走中转）
200 {"state":"ready","mode":"proxy","size":123}
200 {"state":"ready","mode":"direct","size":123,"url":"https://…","expires_in":900}
200 {"state":"missing"}        （也可能是 "uploading"、"blocked"）
```

- 每次返回 `ready` 算一次下载：下载次数加一，“最近下载时间”更新为现在。
- `proxy`：客户端接着请求下面的文件接口。
- `direct`：客户端对 `url` 发 `GET`，**不带** `Authorization`。失败的话客户端会带 `{"mode":"proxy"}` 重新请求一次改走中转。

```
GET {base}/api/v1/models/{id}/file
200 （文件内容，Content-Type: application/octet-stream，带 Content-Length）
206 （带 Range: bytes=N- 的续传）
404 {"error":"not_found"}
```

- 文件接口在任何传输方式下都可用（`direct` 时由服务器从对象存储读出来转发），本身不计下载次数。

## 3. 管理接口

都在 `{base}/api/v1/admin/` 下，鉴权见 1.2。

### 3.1 概况

```
GET /api/v1/admin/overview
200 {"version":"0.1.0","storage":"local","transfer":"proxy",
     "max_file_bytes":209715200,"max_total_bytes":0,"chunk_bytes":5242880,
     "model_count":12,"total_bytes":345678901,"blocked_count":1,"active_uploads":0,
     "retention_hours":72,"last_cleanup_at":1700000000,"next_cleanup_at":1700000600,"now":1700000123,
     "initial_password":false,"disk_free_bytes":23000000000}
```

- `initial_password`：管理员还在用首次启动时生成的初始密码（页面据此提醒尽快修改）。
- `disk_free_bytes`：数据目录所在磁盘的剩余空间；拿不到时是 0。

### 3.2 模型列表

```
GET /api/v1/admin/models?q=&sort=uploaded_at&order=desc&offset=0&limit=50
200 {"total":12,"items":[
  {"id":"…","name":"Chiffon.vrca","size":12345678,"uploader":"昵称",
   "uploaded_at":1700000000,"last_download_at":1700003600,"download_count":3,"expires_at":1700262800}
]}
```

- `q`：对文件名、上传者、id 做不区分大小写的子串匹配。
- `sort`：`uploaded_at`（默认）、`last_download_at`、`size`、`name`、`uploader`、`download_count`、`expires_at`；`order`：`asc` / `desc`（默认）。
- `limit` 1–500，默认 50。
- `last_download_at` 为 0 表示还没人下载过。
- `expires_at` = `max(uploaded_at, last_download_at) + retention_hours*3600`；关闭自动删除时是 0。

### 3.3 删除

```
POST /api/v1/admin/models/delete
{"ids":["…","…"],"block":false,"note":""}
200 {"deleted":2,"blocked":0}
```

- `block: true`：删除的同时把这些 id 加进禁止名单（`note` 记进名单，可空）。之后这些模型不接受上传、不提供下载，直到解除。
- 不带 `block` 的删除只是腾空间：之后还有人需要的话，持有文件的玩家会再传上来。

### 3.4 禁止名单

```
GET  /api/v1/admin/blocklist
200 {"items":[{"id":"…","name":"Chiffon.vrca","note":"…","blocked_at":1700000000}]}

POST /api/v1/admin/blocklist
{"id":"…","name":"","note":""}          手动加一个 id；云端有这个文件的话一并删除
200 {"ok":true}

POST /api/v1/admin/blocklist/remove
{"ids":["…"]}
200 {"removed":1}
```

- `name` 是加入名单时那个模型的文件名（删除并禁止时由服务器从记录里带过来），只为了让管理员认得出是哪个。

### 3.5 设置

所有设置都在这里读写，存在数据目录里，**改完立刻生效，不需要重启**（监听地址、数据目录、程序自带的 HTTPS 是启动参数，不在这里，见第 6 节）。

```
GET /api/v1/admin/settings
200 {
  "retention_hours": 72,
  "base_path": "",
  "auth_key": "…",
  "max_file_mb": 200,
  "max_total_mb": 0,
  "chunk_mb": 5,
  "max_active_uploads": 8,
  "transfer": "proxy",
  "direct_url_ttl_seconds": 900,
  "verify_direct_uploads": true,
  "trust_forwarded_headers": false,
  "storage": {
    "type": "local",
    "s3": {"endpoint":"","region":"auto","bucket":"","access_key_id":"",
           "secret_access_key_set":false,"prefix":"models/","path_style":true}
  }
}

PUT /api/v1/admin/settings
（请求体是上面这个对象的任意一部分：只改带上的那些键，没带的保持原样）
200 （和 GET 一样的完整对象，是改完之后的值）
400 {"error":"invalid_settings","field":"storage.s3.bucket","message":"…"}
409 {"error":"storage_change_needs_confirm","model_count":12,"message":"…"}
400 {"error":"storage_unreachable","message":"…"}
```

各项的含义、取值范围和默认值：

| 键 | 默认 | 说明 |
| :-- | :-- | :-- |
| `retention_hours` | 72 | 一个模型连续这么多小时没有任何人下载就自动删除（从没被下载过的从上传时间算）。整数 0–87600；0 = 不自动删除 |
| `base_path` | 空 | 入口路径前缀。空，或者以 `/` 开头、不以 `/` 结尾，只含字母、数字和 `- _ . ~ /`，最长 64 |
| `auth_key` | 首次启动时随机生成 | 客户端密钥。16–128 个可见 ASCII 字符，不能有空格。改掉之后旧的立刻失效 |
| `max_file_mb` | 200 | 单个文件的上限，1–5000 |
| `max_total_mb` | 0 | 所有文件的总容量上限；0 = 不限 |
| `chunk_mb` | 5 | 上传分片大小，1–64；`transfer` 是 `direct` 时不能小于 5 |
| `max_active_uploads` | 8 | 同时进行的上传会话上限，1–64 |
| `transfer` | `proxy` | `proxy` 或 `direct`；`direct` 只有 `storage.type` 是 `s3` 时可用 |
| `direct_url_ttl_seconds` | 900 | 临时地址的有效期，60–86400 |
| `verify_direct_uploads` | true | 直连上传完成后把内容读回来核对哈希 |
| `trust_forwarded_headers` | false | 在受信任的反向代理后面时打开：用 `X-Forwarded-For` 的最后一项当来源地址，用 `X-Forwarded-Proto` 判断是不是 HTTPS |
| `storage.type` | `local` | `local`（存在数据目录的 `objects/` 下）或 `s3` |
| `storage.s3.endpoint` | 空 | 例如 `https://<账户ID>.r2.cloudflarestorage.com`，只到主机名，不带路径 |
| `storage.s3.region` | `auto` | |
| `storage.s3.bucket` | 空 | |
| `storage.s3.access_key_id` | 空 | |
| `storage.s3.secret_access_key` | 空 | **只写不读**：GET 里不返回它，只用 `secret_access_key_set` 说明有没有设过。PUT 时不带这个键或给空串 = 保持原来的 |
| `storage.s3.prefix` | `models/` | 对象名前缀 |
| `storage.s3.path_style` | true | |

规则：

- **整体校验，整体生效**：请求里任何一项不合法，返回 `invalid_settings`（`field` 是出问题的键，用上表的写法），什么都不改。
- **改 `base_path`**：响应照常返回，并在同一个响应里下发新路径下的会话 Cookie（同时让旧路径下的失效）；
  从下一个请求起只认新前缀。页面拿到响应后跳转到新地址。
- **换存储**（`storage.type` 变了，或者 `type` 是 `s3` 时 `endpoint` / `bucket` / `prefix` / `path_style` / `region` 任何一项变了）：
  现有的模型都留在旧存储里，换过去之后拿不到，所以这等于清空。流程是：
  1. 请求里必须带 `"confirm_storage_change": true`，否则返回 `storage_change_needs_confirm`（`model_count` 是现有的模型数），什么都不改；
  2. 先验证新存储能用（写一个探测对象、读回来、删掉）。不能用就返回 `storage_unreachable`，什么都不改；
  3. 作废所有进行中的上传；删掉旧存储里的全部模型对象（尽力而为，删不掉的只记日志）和全部模型记录；禁止名单保留；
  4. 切换到新存储。之后有人需要时，持有文件的玩家会再传上来。
- 只换 `access_key_id` / `secret_access_key`（存储位置没变）不算换存储：验证能用之后直接生效，不清空。
- `storage.type` 是 `s3` 时，`endpoint`、`bucket`、`access_key_id` 必填，并且必须已经有（或这次带上了）`secret_access_key`。

### 3.6 立即清理、进行中的上传

```
POST /api/v1/admin/cleanup
200 {"deleted":3,"freed_bytes":123456789}

GET /api/v1/admin/uploads
200 {"items":[{"id":"…","name":"…","uploader":"…","size":123,"received":45,"mode":"proxy","started_at":1700000000}]}
```

### 3.7 修改管理员密码

```
POST /api/v1/admin/password
{"current":"…","new":"…"}
200 {"ok":true}
401 {"error":"bad_password"}
400 {"error":"weak_password","message":"…"}
```

- 新密码至少 8 个字符，最长 128。`current` 不对返回 `bad_password`，并且和登录失败一样计入限速。
- 成功后：别的登录会话全部作废，发起请求的这个会话保留；初始密码文件（见第 6 节）删除，`initial_password` 变为 false。

### 3.8 测试存储设置

```
POST /api/v1/admin/storage/test
{"storage": {"type":"s3","s3":{…}}}        和设置里的 storage 同样的结构；secret_access_key 不带或为空 = 用已保存的那个
200 {"ok":true}
200 {"ok":false,"message":"…"}
```

- 不保存任何东西，只验证这组设置能不能用（写一个探测对象、读回来、删掉）。`message` 要让人看得出是地址不通、凭据不对还是桶不存在。

## 4. 管理页

- 地址 `{base}/admin/`（`{base}/admin` 重定向过去）。静态文件由 Go 程序内嵌（`embed`），不需要另外部署。
- 页面用相对地址调接口（从 `{base}/admin/` 出发是 `../api/v1/admin/…`），这样不管 `base_path` 配成什么都能用。
- 页面不能依赖任何外部资源（CDN、在线字体等），没有构建步骤，就是手写的 HTML / CSS / JS。
- 文件名、上传者昵称都是别人填的内容，页面只能当纯文本显示（`textContent`），不能拼进 HTML。
- 页面里有完整的设置界面（3.5 的每一项、3.7 改密码、3.8 测试存储），并且能直接给出发给玩家的 `sync_server.json`：
  内容是 `{"url": "<页面所在的源 + base_path>", "key": "<auth_key>"}`，可以复制，也可以下载成文件。

## 5. 后台任务

- **过期清理**：启动时一次，之后每 10 分钟一次。`retention_hours > 0` 时，删除
  `now - max(uploaded_at, last_download_at) > retention_hours*3600` 的模型（先删存储里的对象，再删记录）。
- **上传会话清理**：超时的会话作废，清掉临时文件；`direct` 模式下中止对象存储上未完成的分片上传。
- **启动时对账**：有记录但存储里没有对象的，删掉记录；存储里有对象但没有记录的，补一条记录
  （文件名记成 `(unknown)`，上传时间取对象的修改时间）。

## 6. 启动参数与首次启动

没有配置文件。启动时只有这几项，命令行参数优先于环境变量：

| 参数 | 环境变量 | 默认 | 说明 |
| :-- | :-- | :-- | :-- |
| `-listen` | `VML_LISTEN` | `:8080` | 监听地址。只让本机的反向代理访问时写 `127.0.0.1:8080` |
| `-data` | `VML_DATA_DIR` | `./data` | 数据目录（相对路径相对于启动时的当前目录）：设置、元数据、上传中的临时文件、本地存储的模型文件都在里面 |
| `-tls-cert`、`-tls-key` | `VML_TLS_CERT`、`VML_TLS_KEY` | 空 | 可选：程序自己提供 HTTPS。两项要么都给要么都不给；不给就是明文 HTTP（由反向代理提供 HTTPS） |
| `-version` | | | 打印版本号后退出 |
| `-reset-admin-password` | | | 生成一个新的管理员密码，写进初始密码文件并打印出来，然后退出。要先把服务停掉再执行（数据文件同一时间只能被一个进程打开） |

其余所有设置见 3.5，都在管理页里改。

**首次启动**（数据目录里还没有设置时）：

- 随机生成客户端密钥 `auth_key`；
- 随机生成管理员的**初始密码**（至少 16 个字符，不用容易看混的字符），存它的哈希；把明文写进 `{数据目录}/initial-admin-password.txt`（权限 600），
  并在启动日志里打印一次（带上这个文件的路径）。管理员用它登录后在管理页里改密码；改掉之后这个文件被删除；
- 其余设置取 3.5 表里的默认值。

之后每次启动：日志里打印监听地址、数据目录、当前的 `base_path`、存储类型和传输方式；**不打印**任何密钥和密码。
还在用初始密码时，每次启动都提醒一句初始密码文件在哪（不再打印密码本身）。

设置里的存储不可用（比如 S3 的凭据失效了）时程序照常启动，管理页照常能进（这样才能把设置改对）；
客户端的上传下载接口在存储恢复之前返回 `503 {"error":"storage_unavailable"}`。

## 7. 仓库布局

```
cmd/vml-sync-server/     程序入口
internal/…               服务端实现（配置、鉴权、存储、元数据、上传会话、清理、HTTP 路由）
web/embed.go             把 web/admin 内嵌进程序（package web，函数 Admin() 返回以 index.html 所在目录为根的 fs.FS）
web/admin/               管理页的静态文件（index.html、样式、脚本）
web/devmock/             只给管理页开发用的假服务（内存里模拟管理接口；不进正式程序）
docs/API.md              这份文档
```
