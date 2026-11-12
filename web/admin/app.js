/*
 * vml-sync-server admin page.
 * Hand-written: no build step, no external resources. Contract: docs/API.md (1, 3, 4, 6).
 *
 * One rule holds for everything that comes from the server (file names, uploader
 * names, notes, ids, error messages): it reaches the page only through textContent,
 * text nodes or setAttribute. Nothing in this file assigns innerHTML or builds markup
 * from strings.
 */
(function () {
  'use strict';

  // The page lives at {base}/admin/, so this resolves to {base}/api/v1/admin/ whatever
  // base_path the server was given.
  const API_BASE = '../api/v1/admin/';

  const ID_RE = /^[0-9a-f]{32}$/;
  const RETENTION_MAX = 87600;
  const RETENTION_PRESETS = [24, 72, 168, 720, 0];
  const PAGE_SIZES = [25, 50, 100, 200];
  const SORT_KEYS = ['uploaded_at', 'last_download_at', 'size', 'name', 'uploader', 'download_count', 'expires_at'];
  const SORT_LABELS = {
    uploaded_at: 'col.uploaded', last_download_at: 'col.lastDl', size: 'col.size', name: 'col.name',
    uploader: 'col.uploader', download_count: 'col.count', expires_at: 'col.expires',
  };
  const TABS = ['models', 'blocklist', 'uploads', 'settings'];

  const POLL_ACTIVE_MS = 3000;    // while an upload is in progress
  const POLL_IDLE_MS = 30000;     // otherwise: one small request, only while the tab is visible
  const POLL_OFFLINE_MS = 10000;  // while the last background refresh failed
  const REQUEST_TIMEOUT_MS = 20000;
  const SLOW_TIMEOUT_MS = 90000;  // saving settings / testing storage: the server talks to the object storage first
  const SOON_SECONDS = 6 * 3600;  // "expires soon" highlight
  const LS_LANG = 'vml-admin-lang';
  const LS_LIMIT = 'vml-admin-page-size';
  const SS_FLASH = 'vml-admin-flash'; // a note for the page that loads after a base_path change

  // The rules of docs/API.md 3.5, checked here only to answer sooner; the server decides.
  const BASE_PATH_RE = /^\/[A-Za-z0-9\-_.~\/]*[A-Za-z0-9\-_.~]$/;
  const AUTH_KEY_RE = /^[\x21-\x7e]{16,128}$/;
  const ENDPOINT_RE = /^https?:\/\/[^\/\s?#@]+$/i;
  const PASSWORD_MIN = 8;
  const PASSWORD_MAX = 128;
  const SYNC_FILE_NAME = 'sync_server.json';
  const SVG_NS = 'http://www.w3.org/2000/svg';

  // ------------------------------------------------------------------ texts

  const I18N = {
    zh: {
      'doc.title': '模型同步管理',
      'brand': '模型同步管理',
      'lang.other': 'English',
      'cancel': '取消',
      'retry': '重试',

      'boot.connecting': '正在连接服务器…',
      'boot.retry': '重试',

      'login.title': '管理员登录',
      'login.desc': '输入管理员密码。',
      'login.firstTime': '第一次使用？初始密码在服务第一次启动时的日志里，也保存在数据目录的 initial-admin-password.txt 里。',
      'login.baseChanged': '入口路径已经改了，请在这个新地址重新登录。',
      'login.firstTime': '第一次使用？初始密码在服务第一次启动时的日志里，也保存在数据目录的 initial-admin-password.txt 里。',
      'login.baseChanged': '入口路径已经改了，请在这个新地址重新登录。',
      'login.password': '密码',
      'login.submit': '登录',
      'login.submitting': '正在登录…',
      'login.empty': '请输入密码。',
      'login.bad': '密码错误。',
      'login.locked': '尝试次数过多，请在 {time}后重试。',
      'login.lockedNoTime': '尝试次数过多，请稍后再试。',
      'login.expired': '登录已过期，请重新登录。',
      'login.loggedOut': '已退出登录。',
      'cd.ms': '{m} 分 {s} 秒',
      'cd.s': '{s} 秒',

      'top.refresh': '刷新',
      'top.refreshed': '已刷新。',
      'top.logout': '退出登录',
      'top.live': '有上传在进行，自动刷新中',
      'conn.lost': '{err}。显示的内容可能不是最新的。',
      'conn.retry': '立即重试',
      'conn.back': '和服务器的通信恢复正常了。',
      'pwb.text': '还在用首次启动时生成的初始密码，请尽快修改。',
      'pwb.go': '去修改密码',

      'ov.heading': '概况',
      'ov.diskFree': '磁盘剩余 {size}',
      'ov.loadFailed': '概况加载失败',
      'ov.models': '模型数量',
      'ov.modelsSub': '单个文件上限 {max}',
      'ov.space': '占用空间',
      'ov.spaceCap': '上限 {max}，已用 {pct}',
      'ov.spaceNoCap': '没有设置总容量上限',
      'ov.blocked': '禁止名单',
      'ov.blockedSub': '禁止上传和下载的模型',
      'ov.uploads': '进行中的上传',
      'ov.uploadsIdle': '现在没有上传',
      'ov.uploadsBusy': '每 3 秒自动刷新',
      'ov.retention': '自动删除',
      'ov.change': '修改',
      'ov.retentionOff': '已关闭',
      'ov.retentionSub': '{span}没有人下载就删除',
      'ov.retentionOffSub': '模型不会被自动删除',
      'ov.storage': '存储 / 传输',
      'ov.transferSub': '传输方式：{mode}',
      'ov.lastCleanup': '上次清理：',
      'ov.nextCleanup': '下次清理：',
      'ov.cleanupNever': '还没有清理过',
      'storage.local': '本地磁盘',
      'storage.s3': 'S3 对象存储',
      'transfer.proxy': '经服务器中转',
      'transfer.direct': '直连对象存储',

      'tab.models': '模型',
      'tab.blocklist': '禁止名单',
      'tab.uploads': '进行中的上传',
      'tab.settings': '设置',

      'col.name': '文件名',
      'col.size': '大小',
      'col.uploader': '上传者',
      'col.uploaded': '上传时间',
      'col.lastDl': '最近下载',
      'col.count': '下载次数',
      'col.expires': '到期',
      'col.id': 'id',
      'col.actions': '操作',
      'col.note': '备注',
      'col.blockedAt': '禁止时间',
      'col.progress': '进度',
      'col.mode': '方式',
      'col.started': '开始时间',

      'm.search': '搜索文件名、上传者或 id',
      'm.sortBy': '排序方式',
      'm.asc': '升序',
      'm.desc': '降序',
      'm.pageSizeLabel': '每页条数',
      'm.pageSize': '每页 {n} 条',
      'm.never': '从未',
      'm.noExpiry': '不过期',
      'm.noName': '（没有文件名）',
      'm.noUploader': '（未知）',
      'm.delete': '删除',
      'm.copyId': '复制 id',
      'm.copied': '已复制 id。',
      'm.copyFailed': '复制失败，请手动复制：{id}',
      'm.selectAll': '选择本页全部',
      'm.selectOne': '选择 {name}',
      'm.selected': '已选 {n} 个',
      'm.deleteSelected': '删除所选',
      'm.clearSelection': '取消选择',
      'm.empty': '云端还没有任何模型。',
      'm.emptySearch': '没有匹配“{q}”的模型。',
      'm.clearSearch': '清除搜索',
      'm.loading': '正在加载…',
      'm.loadFailed': '模型列表加载失败',
      'm.range': '第 {from}–{to} 个，共 {total} 个',
      'm.rangeEmpty': '共 0 个',
      'm.prev': '上一页',
      'm.next': '下一页',
      'm.page': '{page} / {pages}',

      'exp.left': '剩 {span}',
      'exp.due': '已到期，等待清理',

      'del.titleOne': '删除这个模型？',
      'del.titleMany': '删除这 {n} 个模型？',
      'list.more': '…以及另外 {n} 个',
      'del.onlyTitle': '只删除',
      'del.onlyDesc': '只是腾出空间。之后如果还有人需要这个模型，持有文件的玩家会自动把它再传上来。',
      'del.blockTitle': '删除并禁止',
      'del.blockDesc': '服务器不再接受它的上传，也不提供下载，直到你在“禁止名单”里解除。',
      'del.block': '同时禁止再次上传',
      'del.note': '备注（可选）',
      'del.notePh': '例如：为什么禁止',
      'del.confirm': '删除',
      'del.confirmBlock': '删除并禁止',
      'del.working': '正在删除…',
      'del.done': '已删除 {n} 个模型。',
      'del.doneBlock': '已删除 {n} 个模型，{b} 个加入了禁止名单。',
      'del.someGone': '另外 {n} 个已经不在云端。',

      'b.desc': '名单里的模型不接受上传，也不提供下载，直到解除。',
      'b.filter': '筛选 id、文件名或备注',
      'b.add': '按 id 添加',
      'b.selectAll': '选择全部',
      'b.unblock': '解除',
      'b.unblockSelected': '解除所选',
      'b.empty': '禁止名单是空的。',
      'b.emptyFilter': '没有匹配“{q}”的条目。',
      'b.noNote': '—',
      'b.loadFailed': '禁止名单加载失败',
      'b.unblockTitleOne': '解除对这个模型的禁止？',
      'b.unblockTitleMany': '解除对这 {n} 个模型的禁止？',
      'b.unblockDesc': '解除之后，服务器会重新接受它的上传和下载；有人需要时，持有文件的玩家会把它再传上来。',
      'b.unblockConfirm': '解除禁止',
      'b.unblockWorking': '正在解除…',
      'b.unblocked': '已解除 {n} 个。',
      'b.addTitle': '按 id 禁止一个模型',
      'b.addDesc': '禁止之后不接受这个 id 的上传，也不提供下载。云端如果已经有这个文件，会一并删除。',
      'b.addId': '模型 id',
      'b.addIdHint': '32 位十六进制，例如 3f9a…',
      'b.addIdBad': 'id 必须正好是 32 个十六进制字符（0–9、a–f）。',
      'b.addName': '文件名（可选，只为了以后认得出是哪个）',
      'b.addConfirm': '加入禁止名单',
      'b.addWorking': '正在添加…',
      'b.added': '已加入禁止名单。',

      'u.empty': '现在没有进行中的上传。',
      'u.emptySub': '有上传开始后，这里会自动刷新。',
      'u.loadFailed': '上传列表加载失败',
      'mode.proxy': '中转',
      'mode.direct': '直连',

      's.retTitle': '自动删除',
      's.retDesc': '一个模型连续这么多小时没有任何人下载，就自动删除（从没被下载过的，从上传时间算起）。删除只是腾空间：之后还有人需要的话，持有文件的玩家会再传上来。',
      's.retLabel': '时长（小时）',
      's.presets': '常用时长',
      's.retOff': '不自动删除',
      's.retInvalid': '请输入 0 到 87600 之间的整数（0 表示不自动删除）。',
      's.retShorter': '时长缩短后，已经超过新时长的模型会在下一次清理时被删除。',
      's.save': '保存',
      's.saving': '正在保存…',
      's.reset': '还原',
      's.current': '当前：{text}',
      's.saved': '已保存，自动删除时长：{text}。',
      's.savedOff': '已保存，不再自动删除模型。',
      's.loadFailed': '设置加载失败',
      's.cleanTitle': '立即清理',
      's.cleanDesc': '马上删除所有已经超过自动删除时长的模型。服务器平时每 10 分钟会自动清理一次。',
      's.cleanOff': '自动删除现在是关闭的，清理不会删除任何模型。',
      's.cleanBtn': '立即清理',
      's.cleanWorking': '正在清理…',
      's.cleanDone': '清理完成：删除了 {n} 个模型，腾出 {size}。',
      's.cleanNone': '清理完成：没有到期的模型，什么都没删。',
      's.infoTitle': '服务器信息',
      's.version': '版本',
      's.storage': '存储',
      's.transfer': '传输方式',
      's.diskFree': '数据目录所在磁盘的剩余空间',
      's.unlimited': '不限',
      's.aboutNote': '监听地址、数据目录和程序自带的 HTTPS 是启动参数，不在这里改。忘了管理员密码：先停掉服务，带 -reset-admin-password 参数运行一次，它会生成一个新密码。',
      's.nav': '设置的各个部分',
      's.unsaved': '未保存',
      's.unsavedTab': '有还没保存的改动',
      's.fixFirst': '有填得不对的地方，请先改正标出来的项。',
      's.rejectedField': '服务器没有接受“{label}”：{msg}',
      's.rejected': '服务器没有接受这次修改：{msg}',
      's.noReason': '（没有给出原因）',
      's.intRange': '请输入 {min} 到 {max} 之间的整数。',
      's.intMin': '请输入不小于 {min} 的整数。',
      's.required': '这一项必须填。',

      'c.title': '客户端接入',
      'c.desc': '玩家的 Mod 靠“服务器地址 + 客户端密钥”连上这个服务，两样都对才能上传和下载。只发给信得过的人。',
      'c.url': '服务器地址',
      'c.key': '客户端密钥',
      'c.show': '显示密钥',
      'c.hide': '隐藏密钥',
      'c.copyUrl': '复制地址',
      'c.copyKey': '复制密钥',
      'c.copiedUrl': '已复制服务器地址。',
      'c.copiedKey': '已复制客户端密钥。',
      'c.copiedJson': '已复制 sync_server.json 的内容。',
      'c.copyFailed': '复制失败，请手动选中后复制。',
      'c.regen': '更换密钥…',
      'c.regenNote': '怀疑密钥泄露了，或者不想让某个人再用时，换一个新的。',
      'c.jsonDesc': '把这个文件发给朋友。他们在游戏里按 R 打开轮盘 → 设置 → 模型同步 → 打开配置文件夹，把文件放进去即可。',
      'c.jsonMasked': '预览里的密钥是遮住的；下载和复制得到的是完整内容。',
      'c.jsonCopy': '复制内容',
      'c.jsonDownload': '下载文件',
      'c.downloaded': '已生成 sync_server.json，请到浏览器的下载里找。',
      'c.downloadFailed': '这个浏览器下载不了，请改用“复制内容”，自己存成 sync_server.json。',
      'c.regenTitle': '更换客户端密钥',
      'c.regenWarn': '保存的那一刻旧密钥就失效：所有人的 Mod 都会连不上，直到换上新的。之前发出去的 sync_server.json 都要重新发一遍。',
      'c.newKey': '新密钥',
      'c.newKeyHint': '已经随机生成了一个，直接用就行。也可以自己填：16–128 个字符，只能用英文字母、数字和符号，不能有空格。',
      'c.random': '重新随机',
      'c.keyBad': '密钥要 16–128 个字符，只能用英文字母、数字和符号，不能有空格，也不能有中文。',
      'c.keySame': '这和现在的密钥一样。',
      'c.noCrypto': '这个浏览器不能生成随机密钥，请自己填一个。',
      'c.regenConfirm': '更换密钥',
      'c.regenWorking': '正在更换…',
      'c.regenDone': '客户端密钥已更换，旧密钥已经失效。记得把新的 sync_server.json 发给大家。',

      'l.title': '容量与上传',
      'l.maxFile': '单个文件上限（MB）',
      'l.maxFileHint': '1–5000。比这更大的文件不接受上传。',
      'l.maxTotal': '总容量上限（MB）',
      'l.maxTotalHint': '0 = 不限。满了之后不再接受新的上传，直到腾出空间。',
      'l.usage': '现在已用 {used}。',
      'l.usageDisk': '现在已用 {used}，磁盘剩余 {free}。',
      'l.chunk': '上传分片大小（MB）',
      'l.chunkHint': '1–64；传输方式是直连时不能小于 5。一般不用改。',
      'l.chunkDirect': '现在的传输方式是直连，分片不能小于 5 MB。',
      'l.maxUploads': '同时进行的上传数上限',
      'l.maxUploadsHint': '1–64。满了之后，新的上传要等前面的传完。',
      'l.saved': '“容量与上传”已保存。',

      'st.title': '存储',
      'st.desc': '模型文件存在哪里，以及玩家和服务器之间怎么传。',
      'st.type': '文件存在哪里',
      'st.local': '本地磁盘',
      'st.localDesc': '存在服务器数据目录的 objects/ 下。',
      'st.s3': 'S3 兼容的对象存储',
      'st.s3Desc': '例如 Cloudflare R2。',
      'st.moveWarn': '这样改等于换了一个存储：现有的模型都留在旧存储里，换过去之后拿不到，相当于清空（保存时会再确认一次）。禁止名单保留。',
      'st.s3Title': 'S3 设置',
      'st.r2Summary': 'Cloudflare R2 怎么填',
      'st.r2Step1': '在 Cloudflare 控制台的 R2 里创建一个桶，把桶的名字填到“桶”。',
      'st.r2Step2': '在 R2 的 API 令牌管理里创建一个令牌，权限选“对象读和写”。创建后会给出 Access Key ID 和 Secret Access Key（Secret 只显示这一次）。',
      'st.r2Step3': '“地址”填 https://<账户 ID>.r2.cloudflarestorage.com（账户 ID 在 R2 的概览页上）。只到主机名为止，后面不要带桶名。',
      'st.r2Step4': '“区域”填 auto，“路径风格的地址”保持打开。填完先点“测试连接”。',
      'st.endpoint': '地址（endpoint）',
      'st.endpointPh': 'https://<账户ID>.r2.cloudflarestorage.com',
      'st.endpointHint': '只到主机名，不带路径，也不带桶名。',
      'st.endpointBad': '地址要以 https:// 或 http:// 开头，只到主机名为止，后面不能带路径。',
      'st.region': '区域（region）',
      'st.regionHint': 'Cloudflare R2 填 auto。',
      'st.bucket': '桶（bucket）',
      'st.secretSet': '已设置，留空则不修改',
      'st.secretUnset': '还没有设置',
      'st.secretHint': '只能写入，保存之后不会再显示出来。',
      'st.secretRequired': '还没有保存过 Secret Access Key，这一项必须填。',
      'st.prefix': '对象名前缀（prefix）',
      'st.prefixHint': '默认 models/。同一个桶里还放别的东西时，用它隔开。',
      'st.pathStyle': '用路径风格的地址（path style）',
      'st.pathStyleHint': '也就是 https://主机/桶/对象 这种写法。Cloudflare R2、MinIO 保持打开。',
      'st.test': '测试连接',
      'st.testing': '正在测试…',
      'st.testOk': '连接正常：这组设置可以用。',
      'st.testBad': '不能用：{msg}',
      'st.testBadNoMsg': '不能用（服务器没有说明原因）。',
      'st.testNote': '测试只是试着写入、读回、删除一个探测对象，不会保存任何设置。',
      'st.transfer': '传输方式',
      'st.proxy': '经服务器中转（proxy）',
      'st.proxyDesc': '文件内容都经过这台服务器。任何存储都能用。',
      'st.direct': '直连对象存储（direct）',
      'st.directDesc': '玩家凭临时地址直接和对象存储传文件，流量不经过这台服务器。',
      'st.directOff': '直连需要 S3 兼容的对象存储；用本地磁盘时，文件只能经服务器中转。',
      'st.directTitle': '直连的选项',
      'st.ttl': '临时地址的有效期（秒）',
      'st.ttlHint': '60–86400。默认 900（15 分钟）。',
      'st.verify': '直连上传完成后，把内容读回来核对哈希',
      'st.verifyHint': '建议保持打开。关掉之后，服务器不再核对直连传上来的内容。',
      'st.chunkWarn': '直连要求上传分片不小于 5 MB，现在是 {n} MB。请先到“容量与上传”把分片大小改成 5 或更大并保存。',
      'st.unreachable': '新的存储用不了，设置没有保存：{msg}',
      'st.unreachableNoMsg': '新的存储用不了，设置没有保存。',
      'st.confirmTitle': '换存储会清空现有的 {n} 个模型',
      'st.confirmTitleZero': '确认换存储？',
      'st.confirmBody1': '现有的 {n} 个模型都留在旧存储里，换过去之后拿不到，所以会被全部删掉；进行中的上传也会中止。',
      'st.confirmBody1Zero': '云端现在没有模型，不会丢东西。进行中的上传会被中止。',
      'st.confirmBody2': '之后有人需要时，持有文件的玩家会自动重新上传。',
      'st.confirmBody3': '禁止名单保留，不受影响。',
      'st.confirmGo': '换存储并清空',
      'st.confirmGoZero': '换存储',
      'st.confirmWorking': '正在切换…',
      'st.saved': '“存储”已保存。',
      'st.savedMoved': '已经换到新的存储，原来的模型已清空。',
      'st.savedMovedEmpty': '已经换到新的存储。',

      'a.title': '访问与安全',
      'a.basePath': '入口路径（base_path）',
      'a.basePathPh': '留空，或者例如 /x7k2-models',
      'a.basePathHint': '给入口加一段不容易被扫到的路径。留空，或者以 / 开头、不以 / 结尾；只能用字母、数字和 - _ . ~ /，最长 64 个字符。',
      'a.basePathBad': '要么留空，要么以 / 开头、不以 / 结尾，只能用字母、数字和 - _ . ~ /，最长 64 个字符。',
      'a.baseWarn': '改了之后，管理页的地址和发给玩家的地址都会变：旧地址立刻失效，之前发出去的 sync_server.json 要重新发。保存成功后，页面会自动跳到新地址。',
      'a.basePreview': '保存后：管理页 {admin} ，玩家填的服务器地址 {url}',
      'a.trust': '信任反向代理报告的来源地址和协议',
      'a.trustHint': '服务前面有你自己的反向代理（Nginx、Caddy 等），而且只有代理能访问到这个服务时才打开：这样登录限速按真实的来源地址算，也能认出代理那一侧是 HTTPS（依据 X-Forwarded-For 的最后一项和 X-Forwarded-Proto）。服务直接对外时不要开，否则别人可以伪造来源地址。',
      'a.saved': '“访问与安全”已保存。',
      'a.confirmTitle': '更改入口路径？',
      'a.confirmAdmin': '管理页的地址变成：',
      'a.confirmUrl': '玩家要填的服务器地址变成：',
      'a.confirmOld': '旧地址立刻失效，之前发出去的 sync_server.json 都要重新发。',
      'a.confirmProxy': '如果前面有按路径转发的反向代理，先确认新路径也会转发到这个服务，否则保存之后会打不开管理页。',
      'a.confirmGo': '保存并跳转',
      'a.confirmWorking': '正在保存…',
      'a.moved': '入口路径已经改了，现在看到的是新地址。发给玩家的地址也变了，记得重新发 sync_server.json。',

      'pw.title': '管理员密码',
      'pw.initial': '现在用的还是首次启动时生成的初始密码，请改成你自己的。',
      'pw.desc': '修改之后，其他地方已经登录的会话都会被退出，这里保持登录。',
      'pw.current': '当前密码',
      'pw.new': '新密码',
      'pw.repeat': '再输一遍新密码',
      'pw.newHint': '至少 8 个字符，最长 128 个。',
      'pw.currentEmpty': '请输入当前密码。',
      'pw.currentBad': '当前密码不对。',
      'pw.newShort': '新密码至少要 8 个字符。',
      'pw.newLong': '新密码最长 128 个字符。',
      'pw.mismatch': '两次输入的新密码不一样。',
      'pw.weak': '新密码不符合要求',
      'pw.locked': '输错的次数太多，请在 {time}后再试。',
      'pw.lockedNoTime': '输错的次数太多，请稍后再试。',
      'pw.submit': '修改密码',
      'pw.working': '正在修改…',
      'pw.done': '管理员密码已修改。其他已登录的会话都已退出。',

      'd.hours': '{n} 小时',
      'd.days': '{n} 天',
      'd.hoursDays': '{h} 小时（{d} 天）',

      't.justNow': '刚刚',
      't.soon': '马上',
      't.ago.min': '{n} 分钟前',
      't.ago.hour': '{n} 小时前',
      't.ago.day': '{n} 天前',
      't.ago.month': '{n} 个月前',
      't.ago.year': '{n} 年前',
      't.in.min': '{n} 分钟后',
      't.in.hour': '{n} 小时后',
      't.in.day': '{n} 天后',
      't.in.month': '{n} 个月后',
      't.in.year': '{n} 年后',
      'span.dh': '{d} 天 {h} 小时',
      'span.d': '{d} 天',
      'span.hm': '{h} 小时 {m} 分',
      'span.h': '{h} 小时',
      'span.m': '{m} 分钟',
      'span.lt1m': '不到 1 分钟',

      'err.network': '连接不上服务器，请检查网络，或确认服务还在运行',
      'err.timeout': '服务器迟迟没有回应',
      'err.badResponse': '服务器返回了看不懂的内容',
      'err.server': '服务器内部出错（HTTP {status}）',
      'err.http': '请求失败（HTTP {status}）',
      'err.bad_request': '服务器不接受这个请求（格式不对）',
      'err.bad_id': 'id 格式不对',
      'err.not_found': '服务器上没有这个接口（HTTP 404），服务端和这个页面的版本可能不一致',
      'err.too_large': '请求内容太大',
      'err.too_many_attempts': '操作太频繁，请稍后再试',
      'err.invalid_settings': '有设置项不合法',
      'err.storage_unreachable': '存储用不了',
      'err.storage_change_needs_confirm': '换存储需要先确认',
      'err.bad_password': '密码不对',
      'err.weak_password': '新密码不符合要求',
      'err.method_not_allowed': '服务器不接受这种请求（HTTP 405），服务端和这个页面的版本可能不一致',
      'err.sep': '：',
    },

    en: {
      'doc.title': 'Model sync admin',
      'brand': 'Model sync admin',
      'lang.other': '中文',
      'cancel': 'Cancel',
      'retry': 'Retry',

      'boot.connecting': 'Connecting to the server…',
      'boot.retry': 'Retry',

      'login.title': 'Admin sign-in',
      'login.desc': 'Enter the admin password.',
      'login.firstTime': 'First time here? The initial password is in the log of the service\'s first start, and in initial-admin-password.txt in the data directory.',
      'login.baseChanged': 'The entry path has changed. Sign in again at this new address.',
      'login.firstTime': 'First time here? The initial password is in the log of the service\'s first start, and in initial-admin-password.txt in the data directory.',
      'login.baseChanged': 'The entry path has changed. Sign in again at this new address.',
      'login.password': 'Password',
      'login.submit': 'Sign in',
      'login.submitting': 'Signing in…',
      'login.empty': 'Enter the password.',
      'login.bad': 'Wrong password.',
      'login.locked': 'Too many attempts. Try again in {time}.',
      'login.lockedNoTime': 'Too many attempts. Try again later.',
      'login.expired': 'Your session has expired. Sign in again.',
      'login.loggedOut': 'You have signed out.',
      'cd.ms': '{m} min {s} s',
      'cd.s': '{s} s',

      'top.refresh': 'Refresh',
      'top.refreshed': 'Refreshed.',
      'top.logout': 'Sign out',
      'top.live': 'Upload in progress, auto-refreshing',
      'conn.lost': '{err}. What you see may be out of date.',
      'conn.retry': 'Retry now',
      'conn.back': 'The server is answering normally again.',
      'pwb.text': 'You are still using the initial password generated at first start. Change it soon.',
      'pwb.go': 'Change password',

      'ov.heading': 'Overview',
      'ov.diskFree': '{size} free on disk',
      'ov.loadFailed': 'Could not load the overview',
      'ov.models': 'Models',
      'ov.modelsSub': 'Up to {max} per file',
      'ov.space': 'Space used',
      'ov.spaceCap': '{pct} of the {max} cap',
      'ov.spaceNoCap': 'No total size cap set',
      'ov.blocked': 'Blocklist',
      'ov.blockedSub': 'Models barred from upload and download',
      'ov.uploads': 'Uploads in progress',
      'ov.uploadsIdle': 'Nothing uploading right now',
      'ov.uploadsBusy': 'Refreshing every 3 seconds',
      'ov.retention': 'Auto-delete',
      'ov.change': 'Change',
      'ov.retentionOff': 'Off',
      'ov.retentionSub': 'Deleted after {span} without a download',
      'ov.retentionOffSub': 'Models are never deleted automatically',
      'ov.storage': 'Storage / transfer',
      'ov.transferSub': 'Transfer: {mode}',
      'ov.lastCleanup': 'Last cleanup: ',
      'ov.nextCleanup': 'Next cleanup: ',
      'ov.cleanupNever': 'not run yet',
      'storage.local': 'Local disk',
      'storage.s3': 'S3 object storage',
      'transfer.proxy': 'through this server',
      'transfer.direct': 'direct to object storage',

      'tab.models': 'Models',
      'tab.blocklist': 'Blocklist',
      'tab.uploads': 'Uploads in progress',
      'tab.settings': 'Settings',

      'col.name': 'File name',
      'col.size': 'Size',
      'col.uploader': 'Uploader',
      'col.uploaded': 'Uploaded',
      'col.lastDl': 'Last download',
      'col.count': 'Downloads',
      'col.expires': 'Expires',
      'col.id': 'id',
      'col.actions': 'Actions',
      'col.note': 'Note',
      'col.blockedAt': 'Blocked',
      'col.progress': 'Progress',
      'col.mode': 'Mode',
      'col.started': 'Started',

      'm.search': 'Search file name, uploader or id',
      'm.sortBy': 'Sort by',
      'm.asc': 'Ascending',
      'm.desc': 'Descending',
      'm.pageSizeLabel': 'Rows per page',
      'm.pageSize': '{n} per page',
      'm.never': 'Never',
      'm.noExpiry': 'Never expires',
      'm.noName': '(no file name)',
      'm.noUploader': '(unknown)',
      'm.delete': 'Delete',
      'm.copyId': 'Copy id',
      'm.copied': 'Copied the id.',
      'm.copyFailed': 'Could not copy. Copy it by hand: {id}',
      'm.selectAll': 'Select everything on this page',
      'm.selectOne': 'Select {name}',
      'm.selected': '{n} selected',
      'm.deleteSelected': 'Delete selected',
      'm.clearSelection': 'Clear selection',
      'm.empty': 'There are no models on the server yet.',
      'm.emptySearch': 'No models match “{q}”.',
      'm.clearSearch': 'Clear search',
      'm.loading': 'Loading…',
      'm.loadFailed': 'Could not load the model list',
      'm.range': '{from}–{to} of {total}',
      'm.rangeEmpty': '0 models',
      'm.prev': 'Previous page',
      'm.next': 'Next page',
      'm.page': '{page} / {pages}',

      'exp.left': '{span} left',
      'exp.due': 'Expired, awaiting cleanup',

      'del.titleOne': 'Delete this model?',
      'del.titleMany': 'Delete these {n} models?',
      'list.more': '…and {n} more',
      'del.onlyTitle': 'Delete only',
      'del.onlyDesc': 'Just frees space. If someone needs the model again, a player who has the file uploads it again automatically.',
      'del.blockTitle': 'Delete and block',
      'del.blockDesc': 'The server stops accepting uploads of it and stops serving downloads, until you unblock it on the Blocklist tab.',
      'del.block': 'Also block it from being uploaded again',
      'del.note': 'Note (optional)',
      'del.notePh': 'For example: why it is blocked',
      'del.confirm': 'Delete',
      'del.confirmBlock': 'Delete and block',
      'del.working': 'Deleting…',
      'del.done': 'Deleted {n} models.',
      'del.done.one': 'Deleted 1 model.',
      'del.doneBlock': 'Deleted {n} models; {b} added to the blocklist.',
      'del.doneBlock.one': 'Deleted 1 model; {b} added to the blocklist.',
      'del.someGone': '{n} more were already gone.',
      'del.someGone.one': '1 more was already gone.',

      'b.desc': 'Models on this list are not accepted for upload and not served for download until you unblock them.',
      'b.filter': 'Filter by id, file name or note',
      'b.add': 'Add by id',
      'b.selectAll': 'Select all',
      'b.unblock': 'Unblock',
      'b.unblockSelected': 'Unblock selected',
      'b.empty': 'The blocklist is empty.',
      'b.emptyFilter': 'Nothing matches “{q}”.',
      'b.noNote': '—',
      'b.loadFailed': 'Could not load the blocklist',
      'b.unblockTitleOne': 'Unblock this model?',
      'b.unblockTitleMany': 'Unblock these {n} models?',
      'b.unblockDesc': 'Once unblocked, the server accepts uploads and downloads of it again; when someone needs it, a player who has the file uploads it again.',
      'b.unblockConfirm': 'Unblock',
      'b.unblockWorking': 'Unblocking…',
      'b.unblocked': 'Unblocked {n}.',
      'b.addTitle': 'Block a model by id',
      'b.addDesc': 'Once blocked, this id is not accepted for upload and not served for download. If the file is already on the server, it is deleted as well.',
      'b.addId': 'Model id',
      'b.addIdHint': '32 hex characters, e.g. 3f9a…',
      'b.addIdBad': 'The id must be exactly 32 hexadecimal characters (0–9, a–f).',
      'b.addName': 'File name (optional, only so you can recognise it later)',
      'b.addConfirm': 'Add to blocklist',
      'b.addWorking': 'Adding…',
      'b.added': 'Added to the blocklist.',

      'u.empty': 'Nothing is uploading right now.',
      'u.emptySub': 'This list refreshes by itself once an upload starts.',
      'u.loadFailed': 'Could not load the uploads',
      'mode.proxy': 'Proxy',
      'mode.direct': 'Direct',

      's.retTitle': 'Auto-delete',
      's.retDesc': 'A model is deleted automatically once nobody has downloaded it for this many hours (for a model that was never downloaded, counted from its upload). Deleting only frees space: if someone needs it again, a player who has the file uploads it again.',
      's.retLabel': 'Period (hours)',
      's.presets': 'Common periods',
      's.retOff': 'Never delete',
      's.retInvalid': 'Enter a whole number from 0 to 87600 (0 means never delete).',
      's.retShorter': 'With a shorter period, models already past it are deleted at the next cleanup.',
      's.save': 'Save',
      's.saving': 'Saving…',
      's.reset': 'Reset',
      's.current': 'Current: {text}',
      's.saved': 'Saved. Auto-delete period: {text}.',
      's.savedOff': 'Saved. Models are no longer deleted automatically.',
      's.loadFailed': 'Could not load the settings',
      's.cleanTitle': 'Clean up now',
      's.cleanDesc': 'Delete every model that is already past the auto-delete period, right now. The server does this by itself every 10 minutes.',
      's.cleanOff': 'Auto-delete is off, so a cleanup will not delete anything.',
      's.cleanBtn': 'Clean up now',
      's.cleanWorking': 'Cleaning up…',
      's.cleanDone': 'Cleanup done: deleted {n} models, freed {size}.',
      's.cleanDone.one': 'Cleanup done: deleted 1 model, freed {size}.',
      's.cleanNone': 'Cleanup done: nothing had expired, nothing was deleted.',
      's.infoTitle': 'Server',
      's.version': 'Version',
      's.storage': 'Storage',
      's.transfer': 'Transfer',
      's.diskFree': 'Free space on the data directory\'s disk',
      's.unlimited': 'no cap',
      's.aboutNote': 'The listen address, the data directory and the built-in HTTPS are start-up parameters and cannot be changed here. Forgot the admin password? Stop the service and run it once with -reset-admin-password: it generates a new one.',
      's.nav': 'Sections of the settings',
      's.unsaved': 'Unsaved',
      's.unsavedTab': 'There are unsaved changes',
      's.fixFirst': 'Something is filled in wrongly. Fix the marked fields first.',
      's.rejectedField': 'The server did not accept “{label}”: {msg}',
      's.rejected': 'The server did not accept this change: {msg}',
      's.noReason': '(no reason given)',
      's.intRange': 'Enter a whole number from {min} to {max}.',
      's.intMin': 'Enter a whole number, {min} or more.',
      's.required': 'This field is required.',

      'c.title': 'Client access',
      'c.desc': 'A player\'s mod connects with the server address plus the client key; uploads and downloads need both to be right. Give them only to people you trust.',
      'c.url': 'Server address',
      'c.key': 'Client key',
      'c.show': 'Show the key',
      'c.hide': 'Hide the key',
      'c.copyUrl': 'Copy the address',
      'c.copyKey': 'Copy the key',
      'c.copiedUrl': 'Copied the server address.',
      'c.copiedKey': 'Copied the client key.',
      'c.copiedJson': 'Copied the contents of sync_server.json.',
      'c.copyFailed': 'Could not copy. Select the text and copy it by hand.',
      'c.regen': 'Replace the key…',
      'c.regenNote': 'Replace it when it may have leaked, or to shut someone out.',
      'c.jsonDesc': 'Send this file to your friends. In game they press R to open the radial menu, go to Settings → Model sync → Open config folder, and drop the file there.',
      'c.jsonMasked': 'The key is hidden in this preview; the download and the copy contain the whole file.',
      'c.jsonCopy': 'Copy contents',
      'c.jsonDownload': 'Download file',
      'c.downloaded': 'sync_server.json is ready; look in the browser\'s downloads.',
      'c.downloadFailed': 'This browser could not download it. Use “Copy contents” and save it as sync_server.json yourself.',
      'c.regenTitle': 'Replace the client key',
      'c.regenWarn': 'The old key stops working the moment you save: everyone\'s mod loses its connection until it has the new one. Every sync_server.json you have sent out must be sent again.',
      'c.newKey': 'New key',
      'c.newKeyHint': 'A random one has been generated; you can simply use it. Or type your own: 16–128 characters, letters, digits and symbols only, no spaces.',
      'c.random': 'Another random one',
      'c.keyBad': 'The key must be 16–128 characters: ASCII letters, digits and symbols only, no spaces.',
      'c.keySame': 'This is the key in use now.',
      'c.noCrypto': 'This browser cannot generate a random key. Type one yourself.',
      'c.regenConfirm': 'Replace the key',
      'c.regenWorking': 'Replacing…',
      'c.regenDone': 'The client key was replaced; the old one no longer works. Remember to send everyone the new sync_server.json.',

      'l.title': 'Capacity and uploads',
      'l.maxFile': 'Largest file allowed (MB)',
      'l.maxFileHint': '1–5000. Larger files are not accepted.',
      'l.maxTotal': 'Total size cap (MB)',
      'l.maxTotalHint': '0 = no cap. When full, new uploads are refused until space is freed.',
      'l.usage': '{used} in use now.',
      'l.usageDisk': '{used} in use now, {free} free on disk.',
      'l.chunk': 'Upload chunk size (MB)',
      'l.chunkHint': '1–64; not below 5 while the transfer mode is direct. Rarely needs changing.',
      'l.chunkDirect': 'The transfer mode is direct, so a chunk cannot be smaller than 5 MB.',
      'l.maxUploads': 'Uploads allowed at the same time',
      'l.maxUploadsHint': '1–64. Beyond that, a new upload waits for an earlier one to finish.',
      'l.saved': 'Saved “Capacity and uploads”.',

      'st.title': 'Storage',
      'st.desc': 'Where the model files are kept, and how they travel between players and the server.',
      'st.type': 'Where the files are kept',
      'st.local': 'Local disk',
      'st.localDesc': 'Under objects/ in the server\'s data directory.',
      'st.s3': 'S3-compatible object storage',
      'st.s3Desc': 'For example Cloudflare R2.',
      'st.moveWarn': 'This amounts to switching to a different storage: the models stored now stay behind in the old one and cannot be reached afterwards, so it is the same as emptying it (you will be asked again when saving). The blocklist is kept.',
      'st.s3Title': 'S3 settings',
      'st.r2Summary': 'How to fill this in for Cloudflare R2',
      'st.r2Step1': 'In the Cloudflare dashboard, under R2, create a bucket and put its name under “Bucket”.',
      'st.r2Step2': 'In R2\'s API token management, create a token with the “Object Read & Write” permission. It gives you an Access Key ID and a Secret Access Key (the secret is shown only once).',
      'st.r2Step3': 'Endpoint: https://<account id>.r2.cloudflarestorage.com (the account id is on the R2 overview page). Stop at the host name; do not add the bucket.',
      'st.r2Step4': 'Region: auto. Leave “path-style addresses” on. Then press “Test connection”.',
      'st.endpoint': 'Endpoint',
      'st.endpointPh': 'https://<account-id>.r2.cloudflarestorage.com',
      'st.endpointHint': 'The host name only: no path, no bucket.',
      'st.endpointBad': 'The endpoint must start with https:// or http:// and stop at the host name, with no path after it.',
      'st.region': 'Region',
      'st.regionHint': 'For Cloudflare R2: auto.',
      'st.bucket': 'Bucket',
      'st.secretSet': 'Set. Leave empty to keep it',
      'st.secretUnset': 'Not set yet',
      'st.secretHint': 'Write-only: once saved it is never shown again.',
      'st.secretRequired': 'No Secret Access Key has been saved yet, so this is required.',
      'st.prefix': 'Object name prefix',
      'st.prefixHint': 'Default models/. Keeps these files apart from anything else in the bucket.',
      'st.pathStyle': 'Use path-style addresses',
      'st.pathStyleHint': 'That is, https://host/bucket/object. Leave it on for Cloudflare R2 and MinIO.',
      'st.test': 'Test connection',
      'st.testing': 'Testing…',
      'st.testOk': 'It works: these settings can be used.',
      'st.testBad': 'It does not work: {msg}',
      'st.testBadNoMsg': 'It does not work (the server gave no reason).',
      'st.testNote': 'A test only writes, reads back and deletes one probe object. It saves nothing.',
      'st.transfer': 'Transfer mode',
      'st.proxy': 'Through this server (proxy)',
      'st.proxyDesc': 'Every file passes through this server. Works with any storage.',
      'st.direct': 'Direct to object storage (direct)',
      'st.directDesc': 'Players exchange files with the object storage directly, using short-lived addresses; the traffic does not pass through this server.',
      'st.directOff': 'Direct transfer needs S3-compatible object storage; with the local disk, files can only go through this server.',
      'st.directTitle': 'Options for direct transfer',
      'st.ttl': 'Lifetime of a short-lived address (seconds)',
      'st.ttlHint': '60–86400. Default 900 (15 minutes).',
      'st.verify': 'After a direct upload, read the content back and check its hash',
      'st.verifyHint': 'Best left on. When off, the server no longer checks what was uploaded directly.',
      'st.chunkWarn': 'Direct transfer needs upload chunks of at least 5 MB; the chunk size is {n} MB now. Set it to 5 or more under “Capacity and uploads” and save that first.',
      'st.unreachable': 'The new storage cannot be used, so nothing was saved: {msg}',
      'st.unreachableNoMsg': 'The new storage cannot be used, so nothing was saved.',
      'st.confirmTitle': 'Switching storage discards the {n} models stored now',
      'st.confirmTitle.one': 'Switching storage discards the 1 model stored now',
      'st.confirmTitleZero': 'Switch to the other storage?',
      'st.confirmBody1': 'The {n} models stored now stay behind in the old storage and cannot be reached afterwards, so all of them are deleted; uploads in progress are aborted too.',
      'st.confirmBody1.one': 'The 1 model stored now stays behind in the old storage and cannot be reached afterwards, so it is deleted; uploads in progress are aborted too.',
      'st.confirmBody1Zero': 'There are no models on the server now, so nothing is lost. Uploads in progress are aborted.',
      'st.confirmBody2': 'When someone needs a model again, a player who has the file uploads it again automatically.',
      'st.confirmBody3': 'The blocklist is kept as it is.',
      'st.confirmGo': 'Switch and discard',
      'st.confirmGoZero': 'Switch storage',
      'st.confirmWorking': 'Switching…',
      'st.saved': 'Saved “Storage”.',
      'st.savedMoved': 'Switched to the new storage; the models that were stored have been discarded.',
      'st.savedMovedEmpty': 'Switched to the new storage.',

      'a.title': 'Access and security',
      'a.basePath': 'Entry path (base_path)',
      'a.basePathPh': 'Empty, or for example /x7k2-models',
      'a.basePathHint': 'Puts the entrance behind a path that scanners will not guess. Empty, or starting with / and not ending with /; letters, digits and - _ . ~ / only; 64 characters at most.',
      'a.basePathBad': 'Leave it empty, or start with / and do not end with /; letters, digits and - _ . ~ / only; 64 characters at most.',
      'a.baseWarn': 'This changes the address of this admin page and the address players use: the old ones stop working at once, and every sync_server.json you have sent out must be sent again. After a successful save the page moves to the new address by itself.',
      'a.basePreview': 'After saving: admin page {admin} , server address for players {url}',
      'a.trust': 'Trust the source address and protocol reported by a reverse proxy',
      'a.trustHint': 'Turn this on only when your own reverse proxy (Nginx, Caddy, …) sits in front and nothing else can reach the service: the sign-in limit then counts by the real source address, and HTTPS on the proxy side is recognised (from the last entry of X-Forwarded-For, and X-Forwarded-Proto). Leave it off when the service is reachable directly, or anyone can fake their source address.',
      'a.saved': 'Saved “Access and security”.',
      'a.confirmTitle': 'Change the entry path?',
      'a.confirmAdmin': 'The admin page moves to: ',
      'a.confirmUrl': 'The server address for players becomes: ',
      'a.confirmOld': 'The old addresses stop working at once; every sync_server.json you have sent out must be sent again.',
      'a.confirmProxy': 'If a reverse proxy in front forwards by path, make sure it forwards the new path to this service too; otherwise the admin page will be unreachable after saving.',
      'a.confirmGo': 'Save and go there',
      'a.confirmWorking': 'Saving…',
      'a.moved': 'The entry path has changed; this is the new address. The address for players changed as well, so send out sync_server.json again.',

      'pw.title': 'Admin password',
      'pw.initial': 'You are still using the initial password generated at first start. Replace it with your own.',
      'pw.desc': 'Changing it signs out every other session; this one stays signed in.',
      'pw.current': 'Current password',
      'pw.new': 'New password',
      'pw.repeat': 'New password again',
      'pw.newHint': 'At least 8 characters, 128 at most.',
      'pw.currentEmpty': 'Enter the current password.',
      'pw.currentBad': 'The current password is wrong.',
      'pw.newShort': 'The new password needs at least 8 characters.',
      'pw.newLong': 'The new password can be 128 characters at most.',
      'pw.mismatch': 'The two new passwords are not the same.',
      'pw.weak': 'The new password was not accepted',
      'pw.locked': 'Too many wrong attempts. Try again in {time}.',
      'pw.lockedNoTime': 'Too many wrong attempts. Try again later.',
      'pw.submit': 'Change password',
      'pw.working': 'Changing…',
      'pw.done': 'The admin password was changed. Every other session has been signed out.',

      'd.hours': '{n} hours',
      'd.hours.one': '1 hour',
      'd.days': '{n} days',
      'd.days.one': '1 day',
      'd.hoursDays': '{h} hours ({d} days)',
      'd.hoursDays.one': '24 hours (1 day)',

      't.justNow': 'just now',
      't.soon': 'in a moment',
      't.ago.min': '{n} minutes ago',
      't.ago.min.one': '1 minute ago',
      't.ago.hour': '{n} hours ago',
      't.ago.hour.one': '1 hour ago',
      't.ago.day': '{n} days ago',
      't.ago.day.one': '1 day ago',
      't.ago.month': '{n} months ago',
      't.ago.month.one': '1 month ago',
      't.ago.year': '{n} years ago',
      't.ago.year.one': '1 year ago',
      't.in.min': 'in {n} minutes',
      't.in.min.one': 'in 1 minute',
      't.in.hour': 'in {n} hours',
      't.in.hour.one': 'in 1 hour',
      't.in.day': 'in {n} days',
      't.in.day.one': 'in 1 day',
      't.in.month': 'in {n} months',
      't.in.month.one': 'in 1 month',
      't.in.year': 'in {n} years',
      't.in.year.one': 'in 1 year',
      'span.dh': '{d}d {h}h',
      'span.d': '{d}d',
      'span.hm': '{h}h {m}m',
      'span.h': '{h}h',
      'span.m': '{m}m',
      'span.lt1m': 'under 1m',

      'err.network': 'Cannot reach the server. Check the network, or that the service is still running',
      'err.timeout': 'The server took too long to answer',
      'err.badResponse': 'The server sent something this page does not understand',
      'err.server': 'The server ran into an internal error (HTTP {status})',
      'err.http': 'The request failed (HTTP {status})',
      'err.bad_request': 'The server rejected the request (bad format)',
      'err.bad_id': 'The id is not valid',
      'err.not_found': 'The server has no such endpoint (HTTP 404); the server and this page may be different versions',
      'err.too_large': 'The request is too large',
      'err.too_many_attempts': 'Too many attempts. Try again later',
      'err.invalid_settings': 'A setting is not valid',
      'err.storage_unreachable': 'The storage cannot be used',
      'err.storage_change_needs_confirm': 'Switching storage needs to be confirmed first',
      'err.bad_password': 'Wrong password',
      'err.weak_password': 'The new password was not accepted',
      'err.method_not_allowed': 'The server does not take this kind of request (HTTP 405); the server and this page may be different versions',
      'err.sep': ': ',
    },
  };

  // ------------------------------------------------------------------ state

  const state = {
    lang: 'en',
    view: 'boot',
    epoch: 0,            // bumped on every login / logout; answers from an older epoch are dropped
    tab: 'models',
    overview: null,
    clockOffsetMs: 0,    // server clock minus this machine's clock
    trouble: new Map(),  // what the banner is about: source name -> the error of its last failed load
    models: { q: '', sort: 'uploaded_at', order: 'desc', offset: 0, limit: 50, total: 0, items: [], shown: '', loaded: false, loading: false, error: null, seq: 0 },
    selected: new Map(), // id -> name; survives paging and searching
    blocklist: { items: [], filter: '', selected: new Set(), loaded: false, loading: false, error: null, seq: 0 },
    uploads: { items: [], loaded: false, error: null, seq: 0 },
    settings: {
      saved: null,            // the settings object as the server last sent it (docs/API.md 3.5)
      loaded: false,
      error: null,
      seq: 0,
      saving: null,           // the group whose save is on the way
      tried: {},              // group -> a save was attempted, so every problem in it is shown
      fieldErrors: new Map(), // setting key -> what the server said is wrong with it
      groupErrors: {},        // group -> function giving the message under that form
      keyShown: false,
      testing: false,
      test: null,             // result of the last storage test
      cleaning: false,
      cleanResult: null,
    },
    pw: { busy: false, errors: {}, error: null },
    login: { busy: false, lockUntil: 0, error: null },
    del: { items: [], busy: false },
    unblock: { items: [], busy: false },
    add: { busy: false },
    key: { busy: false },
    confirm: { busy: false, spec: null },
    leaving: false,           // on the way to the page under a new base_path
  };

  // ---------------------------------------------------------------- helpers

  function $(id) { return document.getElementById(id); }

  function lsGet(key) {
    try { return window.localStorage.getItem(key); } catch (e) { return null; }
  }
  function lsSet(key, value) {
    try { window.localStorage.setItem(key, value); } catch (e) { /* storage blocked or full: not essential */ }
  }
  function ssSet(key, value) {
    try { window.sessionStorage.setItem(key, value); } catch (e) { /* not essential */ }
  }
  function ssTake(key) {
    try {
      const v = window.sessionStorage.getItem(key);
      window.sessionStorage.removeItem(key);
      return v;
    } catch (e) {
      return null;
    }
  }

  function detectLang() {
    const saved = lsGet(LS_LANG);
    if (saved === 'zh' || saved === 'en') return saved;
    const prefs = (navigator.languages && navigator.languages.length) ? navigator.languages : [navigator.language || 'en'];
    for (const p of prefs) {
      const s = String(p).toLowerCase();
      if (s.indexOf('zh') === 0) return 'zh';
      if (s.indexOf('en') === 0) return 'en';
    }
    return 'en';
  }

  function t(key, vars) {
    let s = I18N[state.lang][key];
    if (s === undefined) s = I18N.en[key];
    if (s === undefined) return key;
    if (!vars) return s;
    return s.replace(/\{(\w+)\}/g, function (m, name) {
      return Object.prototype.hasOwnProperty.call(vars, name) ? String(vars[name]) : m;
    });
  }

  // Like t(), with a count: uses the "<key>.one" text when n is 1 and the language has one.
  function tn(key, n, vars) {
    const v = Object.assign({ n: fmtInt(n) }, vars || {});
    if (n === 1 && I18N[state.lang][key + '.one'] !== undefined) return t(key + '.one', v);
    return t(key, v);
  }

  function num(v) {
    const n = Number(v);
    return isFinite(n) ? n : 0;
  }
  function str(v) { return v === null || v === undefined ? '' : String(v); }

  // Builds an element. Children are appended as text nodes unless they are nodes already.
  function h(tag, attrs) {
    const el = document.createElement(tag);
    if (attrs) {
      for (const k of Object.keys(attrs)) {
        const v = attrs[k];
        if (v === null || v === undefined || v === false) continue;
        if (k === 'class') el.className = v;
        else if (k === 'text') el.textContent = v;
        else el.setAttribute(k, v === true ? '' : String(v));
      }
    }
    for (let i = 2; i < arguments.length; i++) append(el, arguments[i]);
    return el;
  }
  function append(el, kid) {
    if (kid === null || kid === undefined || kid === false) return;
    if (Array.isArray(kid)) { kid.forEach(function (k) { append(el, k); }); return; }
    el.appendChild(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }

  function icon(name) {
    const svg = document.createElementNS(SVG_NS, 'svg');
    svg.setAttribute('class', 'icon');
    svg.setAttribute('aria-hidden', 'true');
    const use = document.createElementNS(SVG_NS, 'use');
    use.setAttribute('href', '#i-' + name);
    svg.appendChild(use);
    return svg;
  }

  function button(cls, label, iconName, onClick) {
    const b = h('button', { type: 'button', class: cls });
    if (iconName) b.appendChild(icon(iconName));
    if (label) b.appendChild(h('span', { text: label }));
    b.addEventListener('click', onClick);
    return b;
  }

  // Text typed by someone else (file name, uploader, note): shown verbatim as text.
  function userText(value, emptyKey) {
    const s = str(value);
    if (s === '') return h('span', { class: 'ut-empty', text: t(emptyKey) });
    return h('bdi', { class: 'ut', text: s, title: s });
  }

  function setShown(el, shown) { el.hidden = !shown; }

  // -------------------------------------------------------------- formatting

  function locale() { return state.lang === 'zh' ? 'zh-CN' : 'en-US'; }
  function fmtInt(n) {
    try { return num(n).toLocaleString(locale()); } catch (e) { return String(num(n)); }
  }

  function fmtBytes(n) {
    n = Number(n);
    if (!isFinite(n) || n < 0) return '—';
    if (n < 1024) return fmtInt(Math.round(n)) + ' B';
    const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
    let v = n / 1024;
    let i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    let text = v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2);
    if (text === '1024' && i < units.length - 1) { text = '1.00'; i++; }
    return text + ' ' + units[i];
  }

  function nowSec() { return (Date.now() + state.clockOffsetMs) / 1000; }
  function validTs(ts) { return typeof ts === 'number' && isFinite(ts) && ts > 0 && ts < 1e11; }

  function pad2(n) { return n < 10 ? '0' + n : String(n); }
  // Full local time, unambiguous in either language: 2026-11-11 14:03:22 (UTC+8)
  function fmtFull(ts) {
    const d = new Date(ts * 1000);
    const off = -d.getTimezoneOffset();
    const sign = off >= 0 ? '+' : '-';
    const oh = Math.floor(Math.abs(off) / 60);
    const om = Math.abs(off) % 60;
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' +
      pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + ':' + pad2(d.getSeconds()) +
      ' (UTC' + sign + oh + (om ? ':' + pad2(om) : '') + ')';
  }

  function fmtRel(ts) {
    const diff = nowSec() - ts;
    const past = diff >= 0;
    const a = Math.abs(diff);
    if (a < 60) return t(past ? 't.justNow' : 't.soon');
    let n, unit;
    if (a < 3600) { n = Math.floor(a / 60); unit = 'min'; }
    else if (a < 86400) { n = Math.floor(a / 3600); unit = 'hour'; }
    else if (a < 86400 * 30) { n = Math.floor(a / 86400); unit = 'day'; }
    else if (a < 86400 * 365) { n = Math.floor(a / (86400 * 30)); unit = 'month'; }
    else { n = Math.floor(a / (86400 * 365)); unit = 'year'; }
    return tn((past ? 't.ago.' : 't.in.') + unit, n);
  }

  // A length of time, two units at most: "2 天 5 小时" / "2d 5h".
  function fmtSpan(sec) {
    sec = Math.max(0, Math.floor(sec));
    const d = Math.floor(sec / 86400);
    const hh = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    if (d > 0) return hh > 0 ? t('span.dh', { d: d, h: hh }) : t('span.d', { d: d });
    if (hh > 0) return m > 0 ? t('span.hm', { h: hh, m: m }) : t('span.h', { h: hh });
    if (m > 0) return t('span.m', { m: m });
    return t('span.lt1m');
  }

  // "72 小时（3 天）", "36 小时", "不自动删除"
  function fmtRetention(hours) {
    if (hours === 0) return t('s.retOff');
    if (hours >= 24 && hours % 24 === 0) {
      const d = hours / 24;
      if (d === 1 && I18N[state.lang]['d.hoursDays.one'] !== undefined) return t('d.hoursDays.one');
      return t('d.hoursDays', { h: fmtInt(hours), d: fmtInt(d) });
    }
    return tn('d.hours', hours);
  }
  // The same period in its largest whole unit: "3 天" / "36 小时"
  function fmtRetentionShort(hours) {
    if (hours >= 24 && hours % 24 === 0) return tn('d.days', hours / 24);
    return tn('d.hours', hours);
  }

  function fmtCountdown(sec) {
    const m = Math.floor(sec / 60);
    const s = sec % 60;
    return m > 0 ? t('cd.ms', { m: m, s: s }) : t('cd.s', { s: s });
  }

  // A <time> element that keeps itself current (see tickTimes). kind: 'rel' or 'left'.
  function timeEl(ts, kind) {
    const el = document.createElement('time');
    el.dataset.ts = String(ts);
    el.dataset.kind = kind;
    try { el.dateTime = new Date(ts * 1000).toISOString(); } catch (e) { /* out of range: leave it off */ }
    paintTime(el);
    return el;
  }
  function paintTime(el) {
    const ts = Number(el.dataset.ts);
    el.title = fmtFull(ts);
    if (el.dataset.kind === 'left') {
      const left = ts - nowSec();
      el.textContent = left <= 0 ? t('exp.due') : t('exp.left', { span: fmtSpan(left) });
      el.classList.toggle('due', left <= 0);
      el.classList.toggle('warn', left > 0 && left < SOON_SECONDS);
    } else {
      el.textContent = fmtRel(ts);
    }
  }
  function tickTimes() {
    if (state.view !== 'app' || document.hidden) return;
    document.querySelectorAll('time[data-ts]').forEach(paintTime);
  }

  function relOr(ts, emptyKey) {
    return validTs(ts) ? timeEl(ts, 'rel') : h('span', { class: 'tag-off', text: t(emptyKey) });
  }

  // -------------------------------------------------------------------- API

  function ApiError(kind, status, code, serverMessage, data) {
    this.kind = kind;            // 'network' | 'timeout' | 'badResponse' | 'http'
    this.status = status || 0;
    this.code = code || '';
    this.serverMessage = serverMessage || '';
    this.data = data || null;
    this.handled = false;        // true once the 401 handler has taken over
  }

  async function api(method, path, body, timeoutMs) {
    const init = {
      method: method,
      credentials: 'same-origin',
      cache: 'no-store',
      headers: { 'Accept': 'application/json' },
    };
    if (method !== 'GET') {
      init.headers['Content-Type'] = 'application/json';
      init.headers['X-VML-Admin'] = '1';
      init.body = JSON.stringify(body === undefined || body === null ? {} : body);
    }
    const ctl = new AbortController();
    init.signal = ctl.signal;
    const timer = setTimeout(function () { ctl.abort(); }, timeoutMs || REQUEST_TIMEOUT_MS);

    let res, text;
    try {
      res = await fetch(API_BASE + path, init);
      text = await res.text();
    } catch (e) {
      throw new ApiError(ctl.signal.aborted ? 'timeout' : 'network');
    } finally {
      clearTimeout(timer);
    }

    let data = null;
    if (text) {
      try { data = JSON.parse(text); } catch (e) { data = null; }
    }
    const isObject = data !== null && typeof data === 'object' && !Array.isArray(data);

    if (res.ok) {
      if (!isObject) throw new ApiError('badResponse', res.status);
      return data;
    }

    const err = new ApiError('http', res.status,
      isObject && typeof data.error === 'string' ? data.error : '',
      isObject && typeof data.message === 'string' ? data.message : '',
      isObject ? data : null);
    // A 401 means the session is gone, except where it is the answer to a password that
    // was just typed: signing in, and the "current password" of a password change.
    if (res.status === 401 && path !== 'login' && !(path === 'password' && err.code === 'bad_password')) {
      err.handled = true;
      onUnauthorized();
    }
    throw err;
  }

  function describeError(e) {
    if (!(e instanceof ApiError)) return String(e && e.message ? e.message : e);
    if (e.kind === 'network') return t('err.network');
    if (e.kind === 'timeout') return t('err.timeout');
    if (e.kind === 'badResponse') return t('err.badResponse');
    let base;
    if (e.status >= 500) base = t('err.server', { status: e.status });
    else if (e.code && I18N.en['err.' + e.code] !== undefined) base = t('err.' + e.code);
    else if (e.status === 404) base = t('err.not_found');
    else base = t('err.http', { status: e.status });
    return e.serverMessage ? base + t('err.sep') + e.serverMessage : base;
  }

  // A failed load of one of the page's data sources. When the server cannot be reached, or
  // a background refresh fails for any reason, the banner goes up (one banner, not a toast
  // per request) and stays until that source loads again; while it is up, the poller
  // retries every source. A failure the user is looking straight at gets a toast instead.
  function reportLoadError(source, e, titleKey, quiet) {
    if (e && e.handled) return;
    const unreachable = e instanceof ApiError && (e.kind === 'network' || e.kind === 'timeout');
    if (unreachable || quiet) {
      state.trouble.set(source, e);
      paintTrouble();
    } else {
      toast('err', t(titleKey) + t('err.sep') + describeError(e));
    }
  }

  function clearTrouble(source) {
    if (!state.trouble.delete(source)) return;
    paintTrouble();
    if (state.trouble.size === 0 && state.view === 'app') toast('ok', t('conn.back'));
  }

  function paintTrouble() {
    const e = state.trouble.size > 0 ? state.trouble.values().next().value : null;
    setShown($('conn-banner'), e !== null && state.view === 'app');
    $('conn-text').textContent = e === null ? '' : t('conn.lost', { err: describeError(e) });
  }

  // ----------------------------------------------------------------- toasts

  const liveToasts = new Map(); // text -> { el, timer }

  function toast(kind, text, ms) {
    const key = kind + '\n' + text;
    const old = liveToasts.get(key);
    if (old) dismissToast(key);
    const el = h('div', { class: 'toast toast-' + kind },
      icon(kind === 'ok' ? 'check' : 'alert'),
      h('div', { class: 'toast-text', text: text }));
    const close = h('button', { type: 'button', class: 'icon-btn', 'aria-label': t('cancel') }, icon('x'));
    close.addEventListener('click', function () { dismissToast(key); });
    el.appendChild(close);
    $('toasts').appendChild(el);
    const timer = setTimeout(function () { dismissToast(key); }, ms || (kind === 'ok' ? 4500 : 10000));
    liveToasts.set(key, { el: el, timer: timer });
  }
  function dismissToast(key) {
    const item = liveToasts.get(key);
    if (!item) return;
    clearTimeout(item.timer);
    item.el.remove();
    liveToasts.delete(key);
  }
  function clearToasts() {
    Array.from(liveToasts.keys()).forEach(dismissToast);
  }

  async function copyText(text) {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
        return true;
      }
    } catch (e) { /* fall through to the old way */ }
    // Plain-HTTP deployments have no navigator.clipboard.
    try {
      const ta = h('textarea', { class: 'clip-helper', readonly: true, 'aria-hidden': 'true', tabindex: '-1' });
      ta.value = text;
      (document.querySelector('dialog[open]') || document.body).appendChild(ta);
      ta.select();
      const ok = document.execCommand('copy');
      ta.remove();
      return ok;
    } catch (e) {
      return false;
    }
  }

  function copyButton(id) {
    const b = h('button', { type: 'button', class: 'icon-btn', title: t('m.copyId'), 'aria-label': t('m.copyId') }, icon('copy'));
    b.addEventListener('click', async function () {
      if (await copyText(id)) {
        toast('ok', t('m.copied'), 2500);
        b.classList.add('is-done');
        b.replaceChildren(icon('check'));
        setTimeout(function () { b.classList.remove('is-done'); b.replaceChildren(icon('copy')); }, 1500);
      } else {
        toast('err', t('m.copyFailed', { id: id }), 20000);
      }
    });
    return b;
  }

  function idCell(id, full) {
    const code = h('code', { text: full || id.length <= 12 ? id : id.slice(0, 8) + '…', title: id });
    return h('div', { class: 'id-cell' }, code, copyButton(id));
  }

  // ------------------------------------------------------------------ views

  function showView(name) {
    state.view = name;
    setShown($('view-boot'), name === 'boot');
    setShown($('view-login'), name === 'login');
    setShown($('view-app'), name === 'app');
    paintTrouble();
  }

  let pendingFlash = null;

  async function boot() {
    showView('boot');
    setShown($('boot-spinner'), true);
    setShown($('boot-text'), true);
    setShown($('boot-error'), false);
    setShown($('boot-retry'), false);
    let session;
    try {
      session = await api('GET', 'session');
    } catch (e) {
      setShown($('boot-spinner'), false);
      setShown($('boot-text'), false);
      $('boot-error').textContent = describeError(e);
      setShown($('boot-error'), true);
      setShown($('boot-retry'), true);
      return;
    }
    // After a base_path change the old page sends the browser here and leaves a note.
    const moved = pendingFlash === 'base';
    pendingFlash = null;
    if (session.authenticated === true) {
      enterApp();
      if (moved) toast('ok', t('a.moved'), 15000);
    } else {
      showLogin(moved ? t('login.baseChanged') : '');
    }
  }

  // ------------------------------------------------------------------ login

  let lockTimer = null;

  function showLogin(notice) {
    state.epoch++;
    stopPolling();
    closeDialogs();
    clearToasts();
    resetData();
    state.trouble.clear();
    state.login.busy = false;
    state.login.error = null;
    $('login-password').value = '';
    $('login-notice').textContent = notice || '';
    setShown($('login-notice'), !!notice);
    showView('login');
    renderAll(); // also empties the tables: nothing from the old session stays in the page
    $('login-password').focus();
  }

  function paintLogin() {
    const L = state.login;
    const left = Math.ceil((L.lockUntil - Date.now()) / 1000);
    const locked = L.lockUntil > 0 && left > 0;
    let msg = '';
    if (locked) msg = t('login.locked', { time: fmtCountdown(left) });
    else if (L.error) msg = L.error.key ? t(L.error.key) : L.error.text;
    $('login-error').textContent = msg;
    setShown($('login-error'), msg !== '');
    $('login-submit').textContent = t(L.busy ? 'login.submitting' : 'login.submit');
    $('login-submit').disabled = L.busy || locked;
  }

  function startLock(seconds) {
    state.login.lockUntil = Date.now() + seconds * 1000;
    state.login.error = null;
    clearInterval(lockTimer);
    lockTimer = setInterval(function () {
      if (Date.now() >= state.login.lockUntil) {
        clearInterval(lockTimer);
        lockTimer = null;
        state.login.lockUntil = 0;
      }
      paintLogin();
    }, 250);
    paintLogin();
  }

  async function submitLogin(ev) {
    ev.preventDefault();
    const L = state.login;
    if (L.busy || (L.lockUntil > 0 && Date.now() < L.lockUntil)) return;
    const password = $('login-password').value;
    if (password === '') {
      L.error = { key: 'login.empty' };
      paintLogin();
      $('login-password').focus();
      return;
    }
    L.busy = true;
    L.error = null;
    setShown($('login-notice'), false);
    paintLogin();
    try {
      await api('POST', 'login', { password: password });
    } catch (e) {
      L.busy = false;
      if (e instanceof ApiError && e.status === 429) {
        const wait = e.data ? Math.ceil(num(e.data.retry_after)) : 0;
        if (wait > 0) startLock(wait);
        else { L.error = { key: 'login.lockedNoTime' }; paintLogin(); }
      } else if (e instanceof ApiError && e.status === 401) {
        L.error = { key: 'login.bad' };
        paintLogin();
        $('login-password').select();
      } else {
        L.error = { text: describeError(e) };
        paintLogin();
      }
      return;
    }
    L.busy = false;
    $('login-password').value = '';
    enterApp();
  }

  // Any admin call answered 401: the session is gone (expired, or the server restarted).
  function onUnauthorized() {
    if (state.view === 'app') showLogin(t('login.expired'));
  }

  async function logout() {
    const b = $('btn-logout');
    b.disabled = true;
    try {
      await api('POST', 'logout');
    } catch (e) {
      b.disabled = false;
      if (e.handled) return; // already back on the login page
      toast('err', describeError(e));
      return;
    }
    b.disabled = false;
    showLogin(t('login.loggedOut'));
  }

  // -------------------------------------------------------------------- app

  function resetData() {
    state.overview = null;
    Object.assign(state.models, { q: '', offset: 0, total: 0, items: [], shown: '', loaded: false, loading: false, error: null });
    state.selected.clear();
    Object.assign(state.blocklist, { items: [], filter: '', loaded: false, loading: false, error: null });
    state.blocklist.selected.clear();
    Object.assign(state.uploads, { items: [], loaded: false, error: null });
    Object.assign(state.settings, {
      saved: null, loaded: false, error: null, saving: null, tried: {}, groupErrors: {},
      keyShown: false, testing: false, test: null, cleaning: false, cleanResult: null,
    });
    state.settings.fieldErrors.clear();
    state.pw = { busy: false, errors: {}, error: null };
    $('m-search').value = '';
    $('b-filter').value = '';
    // Nothing typed into the settings (least of all a secret or a password) outlives the session.
    FIELDS.forEach(function (f) { writeForm(f, undefined); });
    ['pw-current', 'pw-new', 'pw-repeat', 'key-new'].forEach(function (id) { $(id).value = ''; });
  }

  function enterApp() {
    state.epoch++;
    state.login.error = null;
    showView('app');
    setTab(tabFromHash(), false);
    renderAll();
    refreshAll('first');
  }

  function tabFromHash() {
    const name = (window.location.hash || '').replace(/^#/, '');
    return TABS.indexOf(name) >= 0 ? name : 'models';
  }

  function setTab(name, focusPanel) {
    state.tab = name;
    TABS.forEach(function (tab) {
      const on = tab === name;
      const btn = $('tab-' + tab);
      btn.setAttribute('aria-selected', on ? 'true' : 'false');
      btn.tabIndex = on ? 0 : -1;
      setShown($('panel-' + tab), on);
    });
    try {
      if (window.location.hash !== '#' + name) window.history.replaceState(null, '', '#' + name);
    } catch (e) { /* not essential */ }
    if (focusPanel) $('tab-' + name).scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }

  function openTab(name) {
    setTab(name, true);
    if (name !== 'settings' && state.settings.keyShown) {
      state.settings.keyShown = false; // the client key does not stay readable behind another tab
      paintClient();
    }
    if (state.view !== 'app') return;
    if (name === 'blocklist') loadBlocklist({ quiet: state.blocklist.loaded });
    else if (name === 'uploads') loadUploads({ quiet: state.uploads.loaded });
    else if (name === 'settings') loadSettings({ quiet: state.settings.loaded });
    schedulePoll();
  }

  // Loads everything the page shows.
  //   'first' - right after signing in: failures are reported, success is not
  //   'user'  - the refresh button: both are reported
  //   'auto'  - the poller, recovering from a failed refresh: silent, the banner tells
  async function refreshAll(mode) {
    const epoch = state.epoch;
    const b = $('btn-refresh');
    b.classList.add('is-spinning');
    const quiet = mode === 'auto';
    const wantBlocklist = state.tab === 'blocklist' || state.blocklist.loaded || state.trouble.has('blocklist');
    const results = await Promise.all([
      loadOverview({ quiet: quiet, noReact: true }),
      loadModels({ quiet: quiet }),
      loadUploads({ quiet: quiet || state.tab !== 'uploads' }),
      wantBlocklist ? loadBlocklist({ quiet: quiet }) : true,
      loadSettings({ quiet: quiet || state.tab !== 'settings' }),
    ]);
    b.classList.remove('is-spinning');
    if (epoch !== state.epoch) return;
    if (mode === 'user' && results.every(Boolean)) toast('ok', t('top.refreshed'), 2000);
    schedulePoll();
  }

  // After something was changed from this page: bring every affected list up to date.
  async function refreshAfterChange() {
    const epoch = state.epoch;
    await loadOverview({ quiet: true, noReact: true });
    if (epoch !== state.epoch) return;
    const jobs = [loadModels({ quiet: true })];
    if (state.blocklist.loaded || state.tab === 'blocklist') jobs.push(loadBlocklist({ quiet: true }));
    if (uploadsActive() || state.tab === 'uploads') jobs.push(loadUploads({ quiet: true }));
    await Promise.all(jobs);
    if (epoch === state.epoch) schedulePoll();
  }

  // ---------------------------------------------------------------- polling

  let pollTimer = null;
  let lastPollAt = 0;

  function uploadsActive() {
    return (state.overview !== null && num(state.overview.active_uploads) > 0) || state.uploads.items.length > 0;
  }

  function stopPolling() {
    clearTimeout(pollTimer);
    pollTimer = null;
  }

  // Fast while something is uploading, slow otherwise, nothing at all while the tab is hidden.
  function schedulePoll() {
    stopPolling();
    paintLive();
    if (state.view !== 'app' || document.hidden) return;
    const delay = state.trouble.size > 0 ? POLL_OFFLINE_MS : uploadsActive() ? POLL_ACTIVE_MS : POLL_IDLE_MS;
    pollTimer = setTimeout(pollTick, delay);
  }

  async function pollTick() {
    if (state.view !== 'app' || document.hidden) return;
    const epoch = state.epoch;
    lastPollAt = Date.now();
    if (state.trouble.size > 0) {
      // The last refresh failed somewhere: anything on screen may be stale, so retry it all.
      await refreshAll('auto');
      return;
    }
    const ok = await loadOverview({ quiet: true });
    if (epoch !== state.epoch) return;
    if (ok && (uploadsActive() || state.tab === 'uploads')) await loadUploads({ quiet: true });
    if (epoch === state.epoch) schedulePoll();
  }

  function onVisibility() {
    if (state.view !== 'app') return;
    if (document.hidden) { stopPolling(); return; }
    tickTimes();
    if (Date.now() - lastPollAt > POLL_ACTIVE_MS) pollTick();
    else schedulePoll();
  }

  function paintLive() {
    const live = state.view === 'app' && uploadsActive();
    setShown($('live-pill'), live);
  }

  // --------------------------------------------------------------- overview

  let overviewSeq = 0;

  async function loadOverview(opts) {
    const epoch = state.epoch;
    const seq = ++overviewSeq;
    let data;
    try {
      data = await api('GET', 'overview');
    } catch (e) {
      if (epoch === state.epoch) reportLoadError('overview', e, 'ov.loadFailed', opts.quiet);
      return false;
    }
    if (epoch !== state.epoch || seq !== overviewSeq) return false;
    clearTrouble('overview');
    const prev = state.overview;
    state.overview = data;
    if (validTs(data.now)) state.clockOffsetMs = data.now * 1000 - Date.now();
    renderOverview();
    if (prev && !opts.noReact) reactToOverview(prev, data);
    return true;
  }

  // The periodic overview is the only thing polled while idle; when its numbers move,
  // the lists they summarise are reloaded.
  function reactToOverview(prev, next) {
    const retentionChanged = prev.retention_hours !== next.retention_hours;
    if (prev.model_count !== next.model_count || prev.total_bytes !== next.total_bytes || retentionChanged) {
      loadModels({ quiet: true });
    }
    if (prev.blocked_count !== next.blocked_count && state.blocklist.loaded) loadBlocklist({ quiet: true });
    // The overview repeats a few settings. When one of them moved (someone saved settings
    // in another window), the settings shown here are out of date.
    const settingsMoved = ['retention_hours', 'storage', 'transfer', 'max_file_bytes', 'max_total_bytes', 'chunk_bytes']
      .some(function (k) { return prev[k] !== next[k]; });
    if (settingsMoved && state.settings.loaded && state.settings.saving === null) loadSettings({ quiet: true });
  }

  function storageLabel(v) {
    const key = 'storage.' + v;
    return I18N.en[key] !== undefined ? t(key) : str(v) || '—';
  }
  function transferLabel(v) {
    const key = 'transfer.' + v;
    return I18N.en[key] !== undefined ? t(key) + ' (' + v + ')' : str(v) || '—';
  }

  function renderOverview() {
    const o = state.overview;
    const dash = '—';
    if (!o) {
      ['ov-models', 'ov-space', 'ov-blocked', 'ov-uploads', 'ov-retention', 'ov-storage'].forEach(function (id) { $(id).textContent = dash; });
      ['ov-models-sub', 'ov-space-sub', 'ov-blocked-sub', 'ov-uploads-sub', 'ov-retention-sub', 'ov-storage-sub'].forEach(function (id) { $(id).textContent = ''; });
      setShown($('ov-meter'), false);
      setShown($('ov-disk'), false);
      setShown($('pw-banner'), false);
      $('ov-cleanup').replaceChildren();
      $('brand-version').textContent = '';
      ['models', 'blocklist', 'uploads'].forEach(function (k) { $('tabc-' + k).textContent = ''; });
      paintFromOverview();
      paintLive();
      return;
    }

    // Still on the password generated at first start: say so on every tab.
    setShown($('pw-banner'), o.initial_password === true);

    $('brand-version').textContent = o.version ? 'v' + str(o.version) : '';

    $('ov-models').textContent = fmtInt(o.model_count);
    $('ov-models-sub').textContent = num(o.max_file_bytes) > 0 ? t('ov.modelsSub', { max: fmtBytes(o.max_file_bytes) }) : '';

    const used = num(o.total_bytes);
    const cap = num(o.max_total_bytes);
    $('ov-space').textContent = fmtBytes(used);
    if (cap > 0) {
      const pct = used / cap * 100;
      const pctText = (pct >= 10 || pct === 0 ? pct.toFixed(0) : pct.toFixed(1)) + '%';
      $('ov-space-sub').textContent = t('ov.spaceCap', { max: fmtBytes(cap), pct: pctText });
      $('ov-meter-fill').style.width = Math.max(0, Math.min(100, pct)) + '%';
      $('ov-meter').classList.toggle('is-warn', pct >= 80 && pct < 95);
      $('ov-meter').classList.toggle('is-full', pct >= 95);
      setShown($('ov-meter'), true);
    } else {
      $('ov-space-sub').textContent = t('ov.spaceNoCap');
      setShown($('ov-meter'), false);
    }
    const free = num(o.disk_free_bytes); // 0: the server could not find out
    $('ov-disk').textContent = free > 0 ? t('ov.diskFree', { size: fmtBytes(free) }) : '';
    setShown($('ov-disk'), free > 0);

    $('ov-blocked').textContent = fmtInt(o.blocked_count);
    $('ov-blocked-sub').textContent = t('ov.blockedSub');

    const active = num(o.active_uploads);
    $('ov-uploads').textContent = fmtInt(active);
    $('ov-uploads-sub').textContent = t(active > 0 ? 'ov.uploadsBusy' : 'ov.uploadsIdle');

    const hours = num(o.retention_hours);
    if (hours > 0) {
      $('ov-retention').textContent = tn('d.hours', hours);
      $('ov-retention-sub').textContent = t('ov.retentionSub', { span: fmtRetentionShort(hours) });
    } else {
      $('ov-retention').textContent = t('ov.retentionOff');
      $('ov-retention-sub').textContent = t('ov.retentionOffSub');
    }

    $('ov-storage').textContent = storageLabel(o.storage);
    $('ov-storage-sub').textContent = t('ov.transferSub', { mode: transferLabel(o.transfer) });

    $('ov-cleanup').replaceChildren(
      t('ov.lastCleanup'), relOr(o.last_cleanup_at, 'ov.cleanupNever'),
      h('span', { class: 'sep', text: '·' }),
      t('ov.nextCleanup'), validTs(o.next_cleanup_at) ? timeEl(o.next_cleanup_at, 'rel') : '—');

    $('tabc-models').textContent = fmtInt(o.model_count);
    $('tabc-blocklist').textContent = fmtInt(o.blocked_count);
    $('tabc-uploads').textContent = active > 0 ? fmtInt(active) : '';

    paintFromOverview();
    paintLive();
  }

  // ----------------------------------------------------------------- models

  async function loadModels(opts) {
    const m = state.models;
    const epoch = state.epoch;
    const seq = ++m.seq;
    m.loading = true;
    $('m-wrap').classList.add('is-busy');
    if (!m.loaded) renderModels();

    const query = modelsQuery();
    let data = null;
    let error = null;
    try {
      data = await api('GET', 'models?' + query);
    } catch (e) {
      error = e;
    }
    if (epoch !== state.epoch || seq !== m.seq) return false;

    if (error) {
      m.loading = false;
      m.error = error;
      reportLoadError('models', error, 'm.loadFailed', opts.quiet);
      renderModels();
      return false;
    }

    const items = Array.isArray(data.items) ? data.items : [];
    const total = Math.max(num(data.total), items.length);
    if (items.length === 0 && total > 0 && m.offset > 0) {
      // The page we were on no longer exists (rows were deleted): step back to the last one.
      m.offset = Math.floor((total - 1) / m.limit) * m.limit;
      return loadModels(opts);
    }
    clearTrouble('models');
    m.items = items;
    m.total = total;
    m.shown = query;
    m.loaded = true;
    m.loading = false;
    m.error = null;
    renderModels();
    return true;
  }

  function modelsQuery() {
    const m = state.models;
    return new URLSearchParams({
      q: m.q, sort: m.sort, order: m.order, offset: String(m.offset), limit: String(m.limit),
    }).toString();
  }

  function modelRow(it) {
    const id = str(it.id);
    const name = str(it.name);
    const selected = state.selected.has(id);
    const tr = h('tr', { class: selected ? 'is-selected' : null });

    const cb = h('input', { type: 'checkbox', 'aria-label': t('m.selectOne', { name: name || id }) });
    cb.checked = selected;
    cb.addEventListener('change', function () {
      if (cb.checked) state.selected.set(id, name);
      else state.selected.delete(id);
      tr.classList.toggle('is-selected', cb.checked);
      renderSelection();
    });

    const lastDl = num(it.last_download_at);
    const expires = num(it.expires_at);
    const del = button('btn btn-sm btn-danger-ghost', t('m.delete'), null, function () {
      openDeleteDialog([{ id: id, name: name }]);
    });

    append(tr, [
      h('td', { class: 'c-check' }, cb),
      h('td', { class: 'c-name', 'data-label': t('col.name') }, userText(name, 'm.noName')),
      h('td', { class: 'c-size num', 'data-label': t('col.size'), title: fmtInt(it.size) + ' B' }, fmtBytes(it.size)),
      h('td', { class: 'c-uploader', 'data-label': t('col.uploader') }, userText(it.uploader, 'm.noUploader')),
      h('td', { class: 'c-uploaded', 'data-label': t('col.uploaded') }, relOr(num(it.uploaded_at), 'm.never')),
      h('td', { class: 'c-lastdl', 'data-label': t('col.lastDl') }, relOr(lastDl, 'm.never')),
      h('td', { class: 'c-count num', 'data-label': t('col.count') }, fmtInt(it.download_count)),
      h('td', { class: 'c-exp', 'data-label': t('col.expires') },
        validTs(expires) ? timeEl(expires, 'left') : h('span', { class: 'tag-off', text: t('m.noExpiry') })),
      h('td', { class: 'c-id', 'data-label': t('col.id') }, idCell(id, false)),
      h('td', { class: 'c-act' }, del),
    ]);
    return tr;
  }

  function stateBox(el, parts) {
    el.replaceChildren.apply(el, parts);
    setShown(el, parts.length > 0);
  }

  function renderModels() {
    const m = state.models;
    const body = $('m-body');
    const box = $('m-state');

    // header: sort indicators
    document.querySelectorAll('#panel-models th[data-sort]').forEach(function (th) {
      th.setAttribute('aria-sort', th.dataset.sort !== m.sort ? 'none' : m.order === 'asc' ? 'ascending' : 'descending');
    });
    paintMobileSort();

    // A failed refresh of the rows already on screen keeps them (the banner says they may
    // be stale). A failed search / sort / page change must not leave rows that no longer
    // match the controls, so that shows the error instead.
    if (m.error && !m.loading && (!m.loaded || m.shown !== modelsQuery())) {
      body.replaceChildren();
      stateBox(box, [
        h('div', { class: 'state-title', text: t('m.loadFailed') }),
        h('div', { class: 'state-msg', text: describeError(m.error) }),
        button('btn', t('retry'), 'refresh', function () { loadModels({ quiet: false }); }),
      ]);
    } else if (!m.loaded) {
      body.replaceChildren();
      stateBox(box, [h('div', { class: 'spinner' }), h('div', { text: t('m.loading') })]);
    } else if (m.items.length === 0) {
      body.replaceChildren();
      if (m.q !== '') {
        stateBox(box, [
          h('div', { text: t('m.emptySearch', { q: m.q }) }),
          button('btn btn-sm', t('m.clearSearch'), null, function () {
            $('m-search').value = '';
            applySearch('');
          }),
        ]);
      } else {
        stateBox(box, [h('div', { text: t('m.empty') })]);
      }
    } else {
      stateBox(box, []);
      const frag = document.createDocumentFragment();
      m.items.forEach(function (it) { frag.appendChild(modelRow(it)); });
      body.replaceChildren(frag);
    }
    if (!m.loading) $('m-wrap').classList.remove('is-busy');

    // pager
    const from = m.items.length ? m.offset + 1 : 0;
    const to = m.offset + m.items.length;
    $('m-range').textContent = !m.loaded ? '' : m.total === 0 ? t('m.rangeEmpty')
      : t('m.range', { from: fmtInt(from), to: fmtInt(to), total: fmtInt(m.total) });
    const pages = Math.max(1, Math.ceil(m.total / m.limit));
    const page = Math.min(pages, Math.floor(m.offset / m.limit) + 1);
    $('m-page').textContent = m.loaded ? t('m.page', { page: fmtInt(page), pages: fmtInt(pages) }) : '';
    $('m-prev').disabled = !m.loaded || m.offset <= 0;
    $('m-next').disabled = !m.loaded || m.offset + m.limit >= m.total;

    renderSelection();
  }

  function renderSelection() {
    const n = state.selected.size;
    setShown($('m-selbar'), n > 0);
    $('m-selcount').textContent = t('m.selected', { n: fmtInt(n) });
    const items = state.models.items;
    let onPage = 0;
    items.forEach(function (it) { if (state.selected.has(str(it.id))) onPage++; });
    const all = $('m-all');
    all.checked = items.length > 0 && onPage === items.length;
    all.indeterminate = onPage > 0 && onPage < items.length;
    all.disabled = items.length === 0;
  }

  function applySearch(q) {
    const m = state.models;
    if (q === m.q) return;
    m.q = q;
    m.offset = 0;
    loadModels({ quiet: false });
  }

  function setSort(key, order) {
    const m = state.models;
    if (order) {
      m.order = order;
    } else if (m.sort === key) {
      m.order = m.order === 'asc' ? 'desc' : 'asc';
    } else {
      // Text columns start A→Z; numbers and times start with the largest / newest.
      m.order = (key === 'name' || key === 'uploader') ? 'asc' : 'desc';
    }
    m.sort = key;
    m.offset = 0;
    loadModels({ quiet: false });
  }

  function paintMobileSort() {
    const m = state.models;
    const sel = $('m-sort');
    if (sel.options.length !== SORT_KEYS.length) {
      sel.replaceChildren.apply(sel, SORT_KEYS.map(function (k) { return h('option', { value: k }); }));
    }
    Array.from(sel.options).forEach(function (o) { o.textContent = t(SORT_LABELS[o.value]); });
    sel.value = m.sort;
    $('m-order').textContent = (m.order === 'asc' ? '↑ ' : '↓ ') + t(m.order === 'asc' ? 'm.asc' : 'm.desc');
  }

  function paintPageSize() {
    const sel = $('m-limit');
    if (sel.options.length !== PAGE_SIZES.length) {
      sel.replaceChildren.apply(sel, PAGE_SIZES.map(function (n) { return h('option', { value: String(n) }); }));
    }
    Array.from(sel.options).forEach(function (o) { o.textContent = t('m.pageSize', { n: o.value }); });
    sel.value = String(state.models.limit);
  }

  // ---------------------------------------------------------------- dialogs

  function closeDialogs() {
    document.querySelectorAll('dialog[open]').forEach(function (d) { d.close(); });
    state.del.busy = false;
    state.unblock.busy = false;
    state.add.busy = false;
    state.key.busy = false;
    state.confirm.busy = false;
    state.confirm.spec = null;
  }

  function dialogBusy() {
    return state.del.busy || state.unblock.busy || state.add.busy || state.key.busy || state.confirm.busy;
  }

  function fillNameList(ul, items, emptyKey) {
    const max = 6;
    const rows = items.slice(0, max).map(function (x) {
      return h('li', null, userText(x.name, emptyKey), h('code', { text: x.id.slice(0, 8) + '…', title: x.id }));
    });
    if (items.length > max) rows.push(h('li', { class: 'more', text: t('list.more', { n: fmtInt(items.length - max) }) }));
    ul.replaceChildren.apply(ul, rows);
  }

  function formError(id, text) {
    $(id).textContent = text || '';
    setShown($(id), !!text);
  }

  // -- delete

  function openDeleteDialog(items) {
    if (items.length === 0) return;
    state.del.items = items;
    state.del.busy = false;
    $('del-block').checked = false;
    $('del-note').value = '';
    formError('del-error', '');
    paintDeleteDialog();
    $('dlg-delete').showModal();
  }

  function paintDeleteDialog() {
    const D = state.del;
    const n = D.items.length;
    const block = $('del-block').checked;
    $('del-title').textContent = n === 1 ? t('del.titleOne') : t('del.titleMany', { n: fmtInt(n) });
    fillNameList($('del-names'), D.items, 'm.noName');
    $('del-ex-only').classList.toggle('is-active', !block);
    $('del-ex-block').classList.toggle('is-active', block);
    setShown($('del-note-field'), block);
    $('del-confirm').textContent = D.busy ? t('del.working') : t(block ? 'del.confirmBlock' : 'del.confirm');
    $('del-confirm').disabled = D.busy;
    $('del-block').disabled = D.busy;
    $('del-note').disabled = D.busy;
  }

  async function submitDelete(ev) {
    ev.preventDefault();
    const D = state.del;
    if (D.busy || D.items.length === 0) return;
    const block = $('del-block').checked;
    const ids = D.items.map(function (x) { return x.id; });
    const payload = { ids: ids, block: block, note: block ? $('del-note').value.trim() : '' };
    D.busy = true;
    formError('del-error', '');
    paintDeleteDialog();
    let res;
    try {
      res = await api('POST', 'models/delete', payload);
    } catch (e) {
      D.busy = false;
      if (e.handled) return;
      paintDeleteDialog();
      formError('del-error', describeError(e));
      return;
    }
    D.busy = false;
    ids.forEach(function (id) { state.selected.delete(id); });
    $('dlg-delete').close();

    const deleted = num(res.deleted);
    let msg = block ? tn('del.doneBlock', deleted, { b: fmtInt(res.blocked) }) : tn('del.done', deleted);
    if (deleted < ids.length) msg += ' ' + tn('del.someGone', ids.length - deleted);
    toast('ok', msg);
    refreshAfterChange();
  }

  // -- unblock

  function openUnblockDialog(items) {
    if (items.length === 0) return;
    state.unblock.items = items;
    state.unblock.busy = false;
    formError('unb-error', '');
    paintUnblockDialog();
    $('dlg-unblock').showModal();
  }

  function paintUnblockDialog() {
    const U = state.unblock;
    const n = U.items.length;
    $('unb-title').textContent = n === 1 ? t('b.unblockTitleOne') : t('b.unblockTitleMany', { n: fmtInt(n) });
    fillNameList($('unb-names'), U.items, 'm.noName');
    $('unb-confirm').textContent = t(U.busy ? 'b.unblockWorking' : 'b.unblockConfirm');
    $('unb-confirm').disabled = U.busy;
  }

  async function submitUnblock(ev) {
    ev.preventDefault();
    const U = state.unblock;
    if (U.busy || U.items.length === 0) return;
    const ids = U.items.map(function (x) { return x.id; });
    U.busy = true;
    formError('unb-error', '');
    paintUnblockDialog();
    let res;
    try {
      res = await api('POST', 'blocklist/remove', { ids: ids });
    } catch (e) {
      U.busy = false;
      if (e.handled) return;
      paintUnblockDialog();
      formError('unb-error', describeError(e));
      return;
    }
    U.busy = false;
    ids.forEach(function (id) { state.blocklist.selected.delete(id); });
    $('dlg-unblock').close();
    toast('ok', t('b.unblocked', { n: fmtInt(res.removed) }));
    refreshAfterChange();
  }

  // -- add to blocklist by id

  function openAddDialog() {
    state.add.busy = false;
    $('add-id').value = '';
    $('add-name').value = '';
    $('add-note').value = '';
    formError('add-error', '');
    paintAddDialog();
    $('dlg-add').showModal();
    $('add-id').focus();
  }

  function paintAddDialog() {
    $('add-confirm').textContent = t(state.add.busy ? 'b.addWorking' : 'b.addConfirm');
    $('add-confirm').disabled = state.add.busy;
  }

  async function submitAdd(ev) {
    ev.preventDefault();
    const A = state.add;
    if (A.busy) return;
    const id = $('add-id').value.trim().toLowerCase();
    if (!ID_RE.test(id)) {
      formError('add-error', t('b.addIdBad'));
      $('add-id').focus();
      return;
    }
    A.busy = true;
    formError('add-error', '');
    paintAddDialog();
    try {
      await api('POST', 'blocklist', { id: id, name: $('add-name').value.trim(), note: $('add-note').value.trim() });
    } catch (e) {
      A.busy = false;
      if (e.handled) return;
      paintAddDialog();
      formError('add-error', describeError(e));
      return;
    }
    A.busy = false;
    state.selected.delete(id);
    $('dlg-add').close();
    toast('ok', t('b.added'));
    refreshAfterChange();
  }

  // -------------------------------------------------------------- blocklist

  async function loadBlocklist(opts) {
    const b = state.blocklist;
    const epoch = state.epoch;
    const seq = ++b.seq;
    b.loading = true;
    $('b-wrap').classList.add('is-busy');
    if (!b.loaded) renderBlocklist();
    let data = null;
    let error = null;
    try {
      data = await api('GET', 'blocklist');
    } catch (e) {
      error = e;
    }
    if (epoch !== state.epoch || seq !== b.seq) return false;
    b.loading = false;
    if (error) {
      b.error = error;
      reportLoadError('blocklist', error, 'b.loadFailed', opts.quiet);
      renderBlocklist();
      return false;
    }
    clearTrouble('blocklist');
    b.items = Array.isArray(data.items) ? data.items : [];
    b.loaded = true;
    b.error = null;
    // forget selections that are no longer on the list
    const alive = new Set(b.items.map(function (x) { return str(x.id); }));
    Array.from(b.selected).forEach(function (id) { if (!alive.has(id)) b.selected.delete(id); });
    renderBlocklist();
    return true;
  }

  function blocklistVisible() {
    const b = state.blocklist;
    const f = b.filter.trim().toLowerCase();
    if (f === '') return b.items;
    return b.items.filter(function (x) {
      return str(x.id).toLowerCase().indexOf(f) >= 0 || str(x.name).toLowerCase().indexOf(f) >= 0 ||
        str(x.note).toLowerCase().indexOf(f) >= 0;
    });
  }

  function blockRow(x) {
    const b = state.blocklist;
    const id = str(x.id);
    const name = str(x.name);
    const selected = b.selected.has(id);
    const tr = h('tr', { class: selected ? 'is-selected' : null });
    const cb = h('input', { type: 'checkbox', 'aria-label': t('m.selectOne', { name: name || id }) });
    cb.checked = selected;
    cb.addEventListener('change', function () {
      if (cb.checked) b.selected.add(id);
      else b.selected.delete(id);
      tr.classList.toggle('is-selected', cb.checked);
      renderBlockSelection();
    });
    const unblock = button('btn btn-sm', t('b.unblock'), null, function () {
      openUnblockDialog([{ id: id, name: name }]);
    });
    append(tr, [
      h('td', { class: 'c-check' }, cb),
      h('td', { class: 'c-bid', 'data-label': t('col.id') }, idCell(id, true)),
      h('td', { class: 'c-bname', 'data-label': t('col.name') }, userText(name, 'm.noName')),
      h('td', { class: 'c-note', 'data-label': t('col.note') }, userText(x.note, 'b.noNote')),
      h('td', { class: 'c-btime', 'data-label': t('col.blockedAt') }, relOr(num(x.blocked_at), 'b.noNote')),
      h('td', { class: 'c-act' }, unblock),
    ]);
    return tr;
  }

  function renderBlocklist() {
    const b = state.blocklist;
    const body = $('b-body');
    const box = $('b-state');
    const rows = blocklistVisible();

    if (b.error && !b.loading && !b.loaded) {
      body.replaceChildren();
      stateBox(box, [
        h('div', { class: 'state-title', text: t('b.loadFailed') }),
        h('div', { class: 'state-msg', text: describeError(b.error) }),
        button('btn', t('retry'), 'refresh', function () { loadBlocklist({ quiet: false }); }),
      ]);
    } else if (!b.loaded) {
      body.replaceChildren();
      stateBox(box, [h('div', { class: 'spinner' }), h('div', { text: t('m.loading') })]);
    } else if (rows.length === 0) {
      body.replaceChildren();
      stateBox(box, [h('div', { text: b.items.length === 0 ? t('b.empty') : t('b.emptyFilter', { q: b.filter.trim() }) })]);
    } else {
      stateBox(box, []);
      const frag = document.createDocumentFragment();
      rows.forEach(function (x) { frag.appendChild(blockRow(x)); });
      body.replaceChildren(frag);
    }
    if (!b.loading) $('b-wrap').classList.remove('is-busy');
    renderBlockSelection();
  }

  function renderBlockSelection() {
    const b = state.blocklist;
    const n = b.selected.size;
    setShown($('b-selbar'), n > 0);
    $('b-selcount').textContent = t('m.selected', { n: fmtInt(n) });
    const rows = blocklistVisible();
    let on = 0;
    rows.forEach(function (x) { if (b.selected.has(str(x.id))) on++; });
    const all = $('b-all');
    all.checked = rows.length > 0 && on === rows.length;
    all.indeterminate = on > 0 && on < rows.length;
    all.disabled = rows.length === 0;
  }

  // ---------------------------------------------------------------- uploads

  async function loadUploads(opts) {
    const u = state.uploads;
    const epoch = state.epoch;
    const seq = ++u.seq;
    let data = null;
    let error = null;
    try {
      data = await api('GET', 'uploads');
    } catch (e) {
      error = e;
    }
    if (epoch !== state.epoch || seq !== u.seq) return false;
    if (error) {
      u.error = error;
      reportLoadError('uploads', error, 'u.loadFailed', opts.quiet);
      renderUploads();
      return false;
    }
    clearTrouble('uploads');
    u.items = Array.isArray(data.items) ? data.items : [];
    u.loaded = true;
    u.error = null;
    renderUploads();
    paintLive();
    return true;
  }

  function modeLabel(v) {
    const key = 'mode.' + v;
    return I18N.en[key] !== undefined ? t(key) : str(v) || '—';
  }

  function uploadRow(x) {
    const size = num(x.size);
    const received = Math.max(0, Math.min(num(x.received), size || Infinity));
    const pct = size > 0 ? Math.min(100, received / size * 100) : 0;
    const fill = h('div', { class: 'bar-fill' });
    fill.style.width = pct.toFixed(1) + '%';
    const bar = h('div', {
      class: 'bar', role: 'progressbar', 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(pct)),
    }, fill);
    const text = Math.floor(pct) + '% · ' + fmtBytes(received) + ' / ' + fmtBytes(size);
    return h('tr', { 'data-id': str(x.id) },
      h('td', { class: 'c-name', 'data-label': t('col.name') }, userText(x.name, 'm.noName')),
      h('td', { class: 'c-uploader', 'data-label': t('col.uploader') }, userText(x.uploader, 'm.noUploader')),
      h('td', { class: 'c-progress', 'data-label': t('col.progress') },
        h('div', { class: 'progress' }, bar, h('span', { class: 'progress-text', text: text }))),
      h('td', { class: 'c-mode', 'data-label': t('col.mode') }, h('span', { class: 'badge', text: modeLabel(x.mode) })),
      h('td', { class: 'c-uploaded', 'data-label': t('col.started') }, relOr(num(x.started_at), 'b.noNote')),
      h('td', { class: 'c-id', 'data-label': t('col.id') }, idCell(str(x.id), false)));
  }

  function renderUploads() {
    const u = state.uploads;
    const body = $('u-body');
    const box = $('u-state');
    if (u.error && !u.loaded) {
      body.replaceChildren();
      stateBox(box, [
        h('div', { class: 'state-title', text: t('u.loadFailed') }),
        h('div', { class: 'state-msg', text: describeError(u.error) }),
        button('btn', t('retry'), 'refresh', function () { loadUploads({ quiet: false }); }),
      ]);
    } else if (!u.loaded) {
      body.replaceChildren();
      stateBox(box, [h('div', { class: 'spinner' }), h('div', { text: t('m.loading') })]);
    } else if (u.items.length === 0) {
      body.replaceChildren();
      stateBox(box, [h('div', { class: 'state-title', text: t('u.empty') }), h('div', { text: t('u.emptySub') })]);
    } else {
      stateBox(box, []);
      const frag = document.createDocumentFragment();
      u.items.forEach(function (x) { frag.appendChild(uploadRow(x)); });
      body.replaceChildren(frag);
    }
  }

  // --------------------------------------------------------------- settings
  //
  // The settings tab is a column of cards. Four of them are "groups": a form that sends
  // its own changed keys, and nothing else, in one PUT (docs/API.md 3.5). What the server
  // last reported is kept in state.settings.saved; what is being edited lives in the
  // controls themselves. A control whose content differs from the saved value is "dirty".

  const GROUPS = ['retention', 'limits', 'storage', 'access'];

  function radios(name) {
    return Array.from(document.querySelectorAll('input[type="radio"][name="' + name + '"]'));
  }
  function radioValue(name) {
    const on = radios(name).filter(function (r) { return r.checked; })[0];
    return on ? on.value : '';
  }
  function setRadio(name, value) {
    radios(name).forEach(function (r) { r.checked = r.value === value; });
  }
  function formIsS3() { return radioValue('f-storage-type') === 's3'; }
  function formIsDirect() { return radioValue('f-transfer') === 'direct'; }

  // key:  the server's name for the setting; invalid_settings.field uses the same spelling.
  // kind: int | text | secret (write-only) | bool | radio.
  // when: the control is on screen, and part of a save, only while this holds.
  const FIELDS = [
    { key: 'retention_hours', group: 'retention', id: 'f-retention', kind: 'int', min: 0, max: RETENTION_MAX },
    { key: 'max_file_mb', group: 'limits', id: 'f-max-file', kind: 'int', min: 1, max: 5000 },
    { key: 'max_total_mb', group: 'limits', id: 'f-max-total', kind: 'int', min: 0, max: null },
    { key: 'chunk_mb', group: 'limits', id: 'f-chunk', kind: 'int', min: 1, max: 64 },
    { key: 'max_active_uploads', group: 'limits', id: 'f-max-uploads', kind: 'int', min: 1, max: 64 },
    { key: 'storage.type', group: 'storage', name: 'f-storage-type', kind: 'radio' },
    { key: 'storage.s3.endpoint', group: 'storage', id: 'f-s3-endpoint', kind: 'text', when: formIsS3, required: true },
    { key: 'storage.s3.bucket', group: 'storage', id: 'f-s3-bucket', kind: 'text', when: formIsS3, required: true },
    { key: 'storage.s3.region', group: 'storage', id: 'f-s3-region', kind: 'text', when: formIsS3 },
    { key: 'storage.s3.access_key_id', group: 'storage', id: 'f-s3-access-key', kind: 'text', when: formIsS3, required: true },
    { key: 'storage.s3.secret_access_key', group: 'storage', id: 'f-s3-secret', kind: 'secret', when: formIsS3 },
    { key: 'storage.s3.prefix', group: 'storage', id: 'f-s3-prefix', kind: 'text', when: formIsS3 },
    { key: 'storage.s3.path_style', group: 'storage', id: 'f-s3-path-style', kind: 'bool', when: formIsS3 },
    { key: 'transfer', group: 'storage', name: 'f-transfer', kind: 'radio' },
    { key: 'direct_url_ttl_seconds', group: 'storage', id: 'f-ttl', kind: 'int', min: 60, max: 86400, when: formIsDirect },
    { key: 'verify_direct_uploads', group: 'storage', id: 'f-verify', kind: 'bool', when: formIsDirect },
    { key: 'base_path', group: 'access', id: 'f-base-path', kind: 'text' },
    { key: 'trust_forwarded_headers', group: 'access', id: 'f-trust', kind: 'bool' },
  ];
  const FIELD_BY_KEY = {};
  FIELDS.forEach(function (f) { FIELD_BY_KEY[f.key] = f; });
  // The S3 settings that say where the objects are. Changing one means a different storage.
  const S3_PLACE_KEYS = ['endpoint', 'region', 'bucket', 'prefix', 'path_style'];

  function fieldInputs(f) { return f.kind === 'radio' ? radios(f.name) : [document.getElementById(f.id)]; }
  function fieldWrap(f) { return fieldInputs(f)[0].closest('[data-field]'); }
  function fieldLabel(f) {
    const el = fieldWrap(f).querySelector('legend, label[for], .check > span');
    return el ? el.textContent.trim() : f.key;
  }

  // A setting as the server last reported it; undefined when it did not.
  function savedValue(key) {
    let v = state.settings.saved;
    const parts = key.split('.');
    for (let i = 0; i < parts.length; i++) {
      if (v === null || typeof v !== 'object') return undefined;
      v = v[parts[i]];
    }
    return v === null ? undefined : v;
  }

  // The saved value the way a control holds it: a string, or a boolean for a checkbox.
  // The secret is never sent back, so its saved state is always "nothing typed".
  function savedForm(f) {
    if (f.kind === 'secret') return savedValue('storage.s3') === undefined ? undefined : '';
    const v = savedValue(f.key);
    if (v === undefined) return undefined;
    return f.kind === 'bool' ? v === true : String(v);
  }
  function currentForm(f) {
    if (f.kind === 'radio') return radioValue(f.name);
    const el = document.getElementById(f.id);
    if (f.kind === 'bool') return el.checked;
    return f.kind === 'secret' ? el.value : el.value.trim();
  }
  function writeForm(f, v) {
    if (f.kind === 'radio') setRadio(f.name, v === undefined ? '' : v);
    else if (f.kind === 'bool') document.getElementById(f.id).checked = v === true;
    else document.getElementById(f.id).value = v === undefined ? '' : v;
  }

  function fieldKnown(f) { return savedForm(f) !== undefined; }
  function fieldActive(f) { return !f.when || f.when(); }
  // Changed by the user, whether or not the control is on screen right now.
  function fieldTouched(f) { return fieldKnown(f) && currentForm(f) !== savedForm(f); }
  function fieldDirty(f) { return fieldActive(f) && fieldTouched(f); }
  function groupDirty(group) {
    return FIELDS.some(function (f) { return f.group === group && fieldDirty(f); });
  }
  function anyDirty() { return state.settings.loaded && GROUPS.some(groupDirty); }

  // What is wrong with a control's content, in words; '' when nothing is.
  function fieldProblem(f) {
    const v = currentForm(f);
    if (f.kind === 'int') {
      const ok = /^\d{1,15}$/.test(v) && Number(v) >= f.min && (f.max === null || Number(v) <= f.max);
      if (!ok) {
        if (f.key === 'retention_hours') return t('s.retInvalid');
        return f.max === null ? t('s.intMin', { min: f.min }) : t('s.intRange', { min: f.min, max: f.max });
      }
      if (f.key === 'chunk_mb' && Number(v) < 5 && savedValue('transfer') === 'direct') return t('l.chunkDirect');
      return '';
    }
    if (f.key === 'base_path') return v === '' || (v.length <= 64 && BASE_PATH_RE.test(v)) ? '' : t('a.basePathBad');
    if (f.required && v === '') return t('s.required');
    if (f.key === 'storage.s3.endpoint' && !ENDPOINT_RE.test(v)) return t('st.endpointBad');
    if (f.kind === 'secret' && v === '' && savedValue('storage.s3.secret_access_key_set') !== true) return t('st.secretRequired');
    return '';
  }

  // The storage object as the form has it. With S3 every field goes along (the secret only
  // when one was typed), so the server gets one complete description of the storage
  // whichever way it merges a nested object. With the local disk the S3 fields are not on
  // screen and stay as they are on the server.
  function storageFromForm() {
    if (!formIsS3()) return { type: radioValue('f-storage-type') };
    const s3 = {};
    FIELDS.forEach(function (f) {
      if (f.key.indexOf('storage.s3.') !== 0) return;
      const name = f.key.slice('storage.s3.'.length);
      if (f.kind === 'secret') { if (currentForm(f) !== '') s3[name] = currentForm(f); }
      else s3[name] = currentForm(f);
    });
    return { type: 's3', s3: s3 };
  }

  // What saving a group sends: the keys that were changed, and only those.
  function groupPayload(group) {
    const out = {};
    let storage = false;
    FIELDS.forEach(function (f) {
      if (f.group !== group || !fieldDirty(f)) return;
      if (f.key.indexOf('storage.') === 0) storage = true;
      else out[f.key] = f.kind === 'int' ? Number(currentForm(f)) : currentForm(f);
    });
    if (storage) out.storage = storageFromForm();
    return out;
  }

  // Takes a settings object from the server. Controls the user has not touched follow the
  // new values; the ones being edited are left alone, except those of `resetGroup` (the
  // group that was just saved).
  function applySaved(data, resetGroup) {
    const s = state.settings;
    const touched = {};
    FIELDS.forEach(function (f) { touched[f.key] = s.saved !== null && fieldTouched(f); });
    s.saved = data;
    s.loaded = true;
    s.error = null;
    FIELDS.forEach(function (f) {
      if (resetGroup === f.group || !touched[f.key]) writeForm(f, savedForm(f));
    });
  }

  async function loadSettings(opts) {
    const s = state.settings;
    const epoch = state.epoch;
    const seq = ++s.seq;
    let data;
    try {
      data = await api('GET', 'settings');
    } catch (e) {
      if (epoch !== state.epoch) return false;
      s.error = e;
      reportLoadError('settings', e, 's.loadFailed', opts.quiet);
      renderSettings();
      return false;
    }
    if (epoch !== state.epoch) return false;
    clearTrouble('settings');
    // An answer overtaken by a newer request, or by a save, is already out of date.
    if (seq !== s.seq || s.saving !== null) return true;
    applySaved(data, null);
    renderSettings();
    return true;
  }

  // The answer to a PUT is the whole settings object as it now is. Should it ever not be,
  // read the settings again rather than guess.
  async function takeSettings(res, group) {
    if (res && res.storage && typeof res.storage === 'object' && typeof res.auth_key === 'string') {
      state.settings.seq++; // a GET still on its way describes the settings before this save
      applySaved(res, group);
      renderSettings();
      return;
    }
    FIELDS.forEach(function (f) { if (f.group === group) writeForm(f, savedForm(f)); });
    await loadSettings({ quiet: true });
  }

  // ---- painting

  function paintField(f) {
    const s = state.settings;
    const wrap = fieldWrap(f);
    const known = s.loaded && fieldKnown(f);
    let msg = s.fieldErrors.get(f.key) || '';
    if (!msg && known && fieldActive(f) && (fieldTouched(f) || s.tried[f.group])) msg = fieldProblem(f);
    wrap.classList.toggle('is-dirty', known && fieldDirty(f));
    wrap.classList.toggle('is-invalid', msg !== '');
    const err = wrap.querySelector('.field-error');
    err.textContent = msg;
    setShown(err, msg !== '');
    fieldInputs(f).forEach(function (el) {
      el.disabled = !known || s.saving !== null;
      if (msg !== '') el.setAttribute('aria-invalid', 'true');
      else el.removeAttribute('aria-invalid');
    });
  }

  function paintGroup(group) {
    const s = state.settings;
    const form = $('g-' + group);
    const dirty = s.loaded && groupDirty(group);
    form.classList.toggle('is-dirty', dirty);
    setShown(form.querySelector('.js-dirty'), dirty);
    const save = form.querySelector('.js-save');
    save.textContent = t(s.saving === group ? 's.saving' : 's.save');
    save.disabled = !dirty || s.saving !== null;
    setShown(form.querySelector('.js-reset'), dirty && s.saving === null);
    const msg = s.groupErrors[group] ? s.groupErrors[group]() : '';
    const err = form.querySelector('.js-group-error');
    err.textContent = msg;
    setShown(err, msg !== '');
    const dot = document.querySelector('#s-nav [data-goto="' + group + '"] .nav-dot');
    if (dot) setShown(dot, dirty);
  }

  // "72 小时（3 天）", "36 小时", "不自动删除"
  function paintRetention() {
    const s = state.settings;
    const f = FIELD_BY_KEY.retention_hours;
    const input = $('f-retention');
    const saved = savedValue('retention_hours');
    const value = s.loaded && fieldProblem(f) === '' ? Number(currentForm(f)) : null;

    // presets (the buttons are created once, so a click does not lose keyboard focus)
    const chips = $('s-chips');
    if (chips.childElementCount !== RETENTION_PRESETS.length) {
      chips.replaceChildren.apply(chips, RETENTION_PRESETS.map(function (hrs) {
        const chip = h('button', { type: 'button', class: 'chip', 'data-hours': String(hrs) }, h('b', { text: String(hrs) }), h('span'));
        chip.addEventListener('click', function () {
          input.value = String(hrs);
          onFieldEdited(f);
        });
        return chip;
      }));
    }
    Array.from(chips.children).forEach(function (chip) {
      const hrs = Number(chip.dataset.hours);
      chip.lastElementChild.textContent = hrs === 0 ? t('s.retOff') : tn('d.days', hrs / 24);
      chip.classList.toggle('is-active', value === hrs);
      chip.setAttribute('aria-pressed', value === hrs ? 'true' : 'false');
      chip.disabled = input.disabled;
    });

    $('s-human').textContent = value === null ? '' : '= ' + fmtRetention(value);
    const shorter = value !== null && typeof saved === 'number' && value !== saved && value > 0 && (saved === 0 || value < saved);
    $('s-hint').textContent = shorter ? t('s.retShorter') : '';
    setShown($('s-hint'), shorter);
    $('s-current').textContent = typeof saved === 'number' ? t('s.current', { text: fmtRetention(saved) }) : '';
  }

  function paintCleanup() {
    const s = state.settings;
    $('s-clean').textContent = t(s.cleaning ? 's.cleanWorking' : 's.cleanBtn');
    $('s-clean').disabled = s.cleaning;
    setShown($('s-clean-off'), savedValue('retention_hours') === 0);
    const r = s.cleanResult;
    $('s-clean-result').textContent = !r ? '' : r.deleted > 0
      ? tn('s.cleanDone', r.deleted, { size: fmtBytes(r.freed) }) : t('s.cleanNone');
  }

  // A number of megabytes in a unit a person reads more easily, when there is one.
  function mbHuman(f) {
    if (!state.settings.loaded || !fieldKnown(f) || fieldProblem(f) !== '') return '';
    const mb = Number(currentForm(f));
    if (mb === 0) return '= ' + t('s.unlimited');
    return mb >= 1024 ? '= ' + fmtBytes(mb * 1048576) : '';
  }

  function paintLimits() {
    $('l-file-human').textContent = mbHuman(FIELD_BY_KEY.max_file_mb);
    $('l-total-human').textContent = mbHuman(FIELD_BY_KEY.max_total_mb);
  }

  function paintStorage() {
    const s = state.settings;
    // Would saving this put the objects somewhere else? (The server decides; this only warns early.)
    const moving = s.loaded && (fieldDirty(FIELD_BY_KEY['storage.type']) ||
      S3_PLACE_KEYS.some(function (k) { return fieldDirty(FIELD_BY_KEY['storage.s3.' + k]); }));
    setShown($('st-move-warn'), moving);

    $('f-s3-secret').placeholder = !s.loaded ? ''
      : t(savedValue('storage.s3.secret_access_key_set') === true ? 'st.secretSet' : 'st.secretUnset');

    const T = s.testing ? null : s.test;
    const out = $('st-test-result');
    out.textContent = !T ? '' : T.error ? describeError(T.error)
      : T.ok ? t('st.testOk') : T.message ? t('st.testBad', { msg: T.message }) : t('st.testBadNoMsg');
    out.classList.toggle('is-ok', !!T && T.ok);
    out.classList.toggle('is-bad', !!T && !T.ok);
    $('st-test').textContent = t(s.testing ? 'st.testing' : 'st.test');
    $('st-test').disabled = !s.loaded || s.testing || s.saving !== null;

    const chunk = savedValue('chunk_mb');
    const tooSmall = formIsDirect() && typeof chunk === 'number' && chunk < 5;
    $('st-chunk-warn').textContent = tooSmall ? t('st.chunkWarn', { n: chunk }) : '';
    setShown($('st-chunk-warn'), tooSmall);
  }

  function paintAccess() {
    const f = FIELD_BY_KEY.base_path;
    const changing = state.settings.loaded && fieldDirty(f) && fieldProblem(f) === '';
    const root = window.location.origin + currentForm(f);
    setShown($('a-base-warn'), changing);
    $('a-base-preview').textContent = changing ? t('a.basePreview', { admin: root + '/admin/', url: root }) : '';
  }

  // The parts of the settings tab that show numbers from the overview.
  function paintFromOverview() {
    const o = state.overview;
    const free = o ? num(o.disk_free_bytes) : 0;
    $('l-usage').textContent = !o ? '' : free > 0
      ? t('l.usageDisk', { used: fmtBytes(o.total_bytes), free: fmtBytes(free) })
      : t('l.usage', { used: fmtBytes(o.total_bytes) });
    setShown($('pw-initial'), !!o && o.initial_password === true);

    const dl = $('s-info');
    if (!o) { dl.replaceChildren(); return; }
    const rows = [
      ['s.version', str(o.version) || '—'],
      ['s.storage', storageLabel(o.storage)],
      ['s.transfer', transferLabel(o.transfer)],
    ];
    if (free > 0) rows.push(['s.diskFree', fmtBytes(free)]);
    const kids = [];
    rows.forEach(function (r) {
      kids.push(h('dt', { text: t(r[0]) }));
      kids.push(h('dd', { text: r[1] }));
    });
    dl.replaceChildren.apply(dl, kids);
  }

  function renderSettings() {
    const s = state.settings;

    const failed = !s.loaded && s.error !== null;
    $('s-load-error-text').textContent = failed ? t('s.loadFailed') + t('err.sep') + describeError(s.error) : '';
    setShown($('s-load-error'), failed);

    // Parts that depend on a choice above them. Direct transfer needs object storage, so
    // choosing the local disk takes "direct" away.
    if (s.loaded && !formIsS3() && formIsDirect()) setRadio('f-transfer', 'proxy');
    const s3 = formIsS3();
    setShown($('st-s3'), s3);
    setShown($('st-direct'), formIsDirect());
    setShown($('st-direct-off'), s.loaded && !s3);

    FIELDS.forEach(paintField);
    radios('f-transfer').forEach(function (r) { if (r.value === 'direct' && !s3) r.disabled = true; });
    GROUPS.forEach(paintGroup);
    const dot = $('tab-settings-dot');
    setShown(dot, anyDirty());
    dot.title = t('s.unsavedTab');

    paintRetention();
    paintLimits();
    paintStorage();
    paintAccess();
    paintClient();
    paintCleanup();
    paintPassword();
  }

  // ---- editing and saving a group

  function onFieldEdited(f) {
    const s = state.settings;
    s.fieldErrors.delete(f.key);
    GROUPS.forEach(function (g) {
      // its own form's message, and any other form's message that was about this setting
      if (g === f.group || (s.groupErrors[g] && s.groupErrors[g].field === f.key)) delete s.groupErrors[g];
    });
    if (f.group === 'storage') s.test = null; // it was about the settings as they were
    renderSettings();
  }

  function resetGroup(group) {
    const s = state.settings;
    FIELDS.forEach(function (f) {
      if (f.group !== group) return;
      writeForm(f, savedForm(f));
      s.fieldErrors.delete(f.key);
    });
    s.tried[group] = false;
    delete s.groupErrors[group];
    if (group === 'storage') s.test = null;
    renderSettings();
  }

  function goToSetting(name, focusId) {
    if (state.tab !== 'settings') openTab('settings');
    $('g-' + name).scrollIntoView({ block: 'start' });
    if (focusId) $(focusId).focus({ preventScroll: true });
  }

  function focusField(f) {
    if (state.tab !== 'settings') openTab('settings');
    const wrap = fieldWrap(f);
    if (wrap.closest('[hidden]')) { $('g-' + f.group).scrollIntoView({ block: 'start' }); return; }
    wrap.scrollIntoView({ block: 'center' });
    const el = fieldInputs(f).filter(function (x) { return !x.disabled; })[0];
    if (el) el.focus({ preventScroll: true });
  }

  function revealGroupError(group) {
    if (state.tab !== 'settings') openTab('settings');
    $('g-' + group).querySelector('.js-group-error').scrollIntoView({ block: 'center' });
  }

  // Checks a group before it is sent. On a problem: shows every one of them, goes to the first.
  function validateGroup(group) {
    const s = state.settings;
    const bad = FIELDS.filter(function (f) {
      return f.group === group && fieldKnown(f) && fieldActive(f) && fieldProblem(f) !== '';
    });
    if (bad.length === 0) return true;
    s.tried[group] = true;
    s.groupErrors[group] = function () { return t('s.fixFirst'); };
    renderSettings();
    focusField(bad[0]);
    return false;
  }

  function submitGroup(group) {
    const s = state.settings;
    if (!s.loaded || s.saving !== null || !groupDirty(group)) return;
    if (!validateGroup(group)) return;
    if (group === 'access' && fieldDirty(FIELD_BY_KEY.base_path)) { openBaseConfirm(); return; }
    saveGroup(group, false);
  }

  function savedMessage(group, moved, discarded) {
    if (group === 'retention') {
      const hours = num(savedValue('retention_hours'));
      return hours === 0 ? t('s.savedOff') : t('s.saved', { text: fmtRetention(hours) });
    }
    if (group === 'storage') return t(!moved ? 'st.saved' : discarded > 0 ? 'st.savedMoved' : 'st.savedMovedEmpty');
    return t(group === 'limits' ? 'l.saved' : 'a.saved');
  }

  // Sends a group's changes. Resolves true once the server has taken them.
  // `discarded`: with a confirmed storage change, how many models the server said it holds.
  async function saveGroup(group, confirmStorageChange, discarded) {
    const s = state.settings;
    if (!s.loaded || s.saving !== null || !validateGroup(group)) return false;
    const payload = groupPayload(group);
    if (Object.keys(payload).length === 0) return false;
    if (confirmStorageChange) payload.confirm_storage_change = true;
    const baseBefore = str(savedValue('base_path'));
    const epoch = state.epoch;

    s.saving = group;
    delete s.groupErrors[group];
    FIELDS.forEach(function (f) { if (f.group === group) s.fieldErrors.delete(f.key); });
    renderSettings();

    let res;
    try {
      res = await api('PUT', 'settings', payload, SLOW_TIMEOUT_MS);
    } catch (e) {
      if (epoch !== state.epoch) return false; // signed out meanwhile: the form is gone
      s.saving = null;
      onSaveError(group, e);
      return false;
    }
    if (epoch !== state.epoch) return false;

    const baseAfter = typeof res.base_path === 'string' ? res.base_path
      : typeof payload.base_path === 'string' ? payload.base_path : baseBefore;
    if (baseAfter !== baseBefore) {
      // From the next request on only the new prefix exists, and the server has put the
      // session cookie under it: nothing more can be asked from this page.
      leaveForBase(baseAfter);
      return true;
    }

    s.saving = null;
    s.tried[group] = false;
    if (group === 'storage') s.test = null;
    await takeSettings(res, group);
    if (epoch !== state.epoch) return true;
    toast('ok', savedMessage(group, confirmStorageChange, discarded));
    refreshAfterChange();
    return true;
  }

  function onSaveError(group, e) {
    const s = state.settings;
    const code = e instanceof ApiError ? e.code : '';
    const msg = e instanceof ApiError ? e.serverMessage : '';

    if (code === 'storage_change_needs_confirm') {
      renderSettings();
      openStorageConfirm(group, e.data ? num(e.data.model_count) : 0);
      return;
    }
    if (code === 'invalid_settings') {
      const key = e.data && typeof e.data.field === 'string' ? e.data.field : '';
      const f = FIELD_BY_KEY[key];
      if (f) {
        // The message goes next to the control it is about, wherever on the tab that is,
        // and under the form that was being saved.
        s.fieldErrors.set(f.key, msg || t('err.invalid_settings'));
        s.groupErrors[group] = function () {
          return t('s.rejectedField', { label: fieldLabel(f), msg: msg || t('s.noReason') });
        };
        s.groupErrors[group].field = f.key; // goes away once that setting is edited
        renderSettings();
        focusField(f);
        return;
      }
      s.groupErrors[group] = function () {
        return t('s.rejected', { msg: (key ? key + t('err.sep') : '') + (msg || t('s.noReason')) });
      };
    } else if (code === 'storage_unreachable') {
      s.groupErrors[group] = function () { return msg ? t('st.unreachable', { msg: msg }) : t('st.unreachableNoMsg'); };
    } else {
      s.groupErrors[group] = function () { return describeError(e); };
    }
    renderSettings();
    revealGroupError(group);
  }

  function openStorageConfirm(group, count) {
    openConfirm({
      danger: true,
      title: function () { return count > 0 ? tn('st.confirmTitle', count) : t('st.confirmTitleZero'); },
      body: function () {
        return [h('ul', null,
          h('li', { text: count > 0 ? tn('st.confirmBody1', count) : t('st.confirmBody1Zero') }),
          h('li', { text: t('st.confirmBody2') }),
          h('li', { text: t('st.confirmBody3') }))];
      },
      label: function () { return t(count > 0 ? 'st.confirmGo' : 'st.confirmGoZero'); },
      working: function () { return t('st.confirmWorking'); },
      run: function () { return saveGroup(group, true, count); },
    });
  }

  function openBaseConfirm() {
    const root = window.location.origin + currentForm(FIELD_BY_KEY.base_path);
    openConfirm({
      danger: false,
      title: function () { return t('a.confirmTitle'); },
      body: function () {
        return [h('ul', null,
          h('li', null, t('a.confirmAdmin'), h('code', { class: 'value-code', text: root + '/admin/' })),
          h('li', null, t('a.confirmUrl'), h('code', { class: 'value-code', text: root })),
          h('li', { text: t('a.confirmOld') }),
          h('li', { text: t('a.confirmProxy') }))];
      },
      label: function () { return t('a.confirmGo'); },
      working: function () { return t('a.confirmWorking'); },
      run: function () { return saveGroup('access', false); },
    });
  }

  // base_path was changed: this page's own address no longer exists. Go to the new one;
  // the note left behind makes the page that loads there say what happened.
  function leaveForBase(base) {
    state.leaving = true;
    state.epoch++; // whatever is still on its way was asked of the old address
    stopPolling();
    ssSet(SS_FLASH, 'base');
    window.location.assign(window.location.origin + base + '/admin/#settings');
  }

  // ---- a confirmation with a custom text

  function openConfirm(spec) {
    state.confirm.spec = spec;
    state.confirm.busy = false;
    paintConfirm();
    if (!$('dlg-confirm').open) $('dlg-confirm').showModal();
  }

  function paintConfirm() {
    const C = state.confirm;
    if (!C.spec) return;
    $('cf-title').textContent = C.spec.title();
    const body = $('cf-body');
    body.replaceChildren.apply(body, C.spec.body());
    const b = $('cf-confirm');
    b.textContent = C.busy ? C.spec.working() : C.spec.label();
    b.className = 'btn ' + (C.spec.danger ? 'btn-danger' : 'btn-primary');
    b.disabled = C.busy;
  }

  async function submitConfirm(ev) {
    ev.preventDefault();
    const C = state.confirm;
    const spec = C.spec;
    if (C.busy || !spec) return;
    C.busy = true;
    paintConfirm();
    try {
      await spec.run(); // reports its own outcome, on the page behind this dialog
    } finally {
      if (C.spec === spec) {
        C.busy = false;
        C.spec = null;
        if ($('dlg-confirm').open) $('dlg-confirm').close();
      }
    }
  }

  // ---- client access: address, key, sync_server.json

  // What players enter as the server address: where this page is served from, plus base_path.
  function clientUrl() { return window.location.origin + str(savedValue('base_path')); }
  function authKey() { return str(savedValue('auth_key')); }
  function maskOf(key) { return '•'.repeat(Math.max(8, Math.min(24, key.length))); }
  function syncJson(key) { return JSON.stringify({ url: clientUrl(), key: key }, null, 2) + '\n'; }

  function paintClient() {
    const s = state.settings;
    const ready = s.loaded && typeof savedValue('auth_key') === 'string';
    const shown = ready && s.keyShown;
    const key = authKey();
    $('c-url').textContent = ready ? clientUrl() : '—';
    $('c-key').textContent = !ready ? '—' : shown ? key : maskOf(key);
    const toggle = $('c-key-toggle');
    toggle.title = t(shown ? 'c.hide' : 'c.show');
    toggle.setAttribute('aria-label', toggle.title);
    toggle.setAttribute('aria-pressed', shown ? 'true' : 'false');
    $('c-key-toggle-icon').setAttribute('href', shown ? '#i-eye-off' : '#i-eye');
    $('c-json').textContent = ready ? syncJson(shown ? key : maskOf(key)).trim() : '';
    setShown($('c-json-note'), ready && !shown);
    ['c-url-copy', 'c-key-toggle', 'c-key-copy', 'c-regen', 'c-json-copy', 'c-json-download'].forEach(function (id) {
      $(id).disabled = !ready;
    });
  }

  async function copyWithToast(text, okKey) {
    if (await copyText(text)) toast('ok', t(okKey), 2500);
    else toast('err', t('c.copyFailed'));
  }

  function downloadSyncFile() {
    let url;
    try {
      url = URL.createObjectURL(new Blob([syncJson(authKey())], { type: 'application/json' }));
      const a = h('a', { href: url, download: SYNC_FILE_NAME, class: 'clip-helper', 'aria-hidden': 'true', tabindex: '-1' });
      document.body.appendChild(a);
      a.click();
      a.remove();
    } catch (e) {
      toast('err', t('c.downloadFailed'));
      return;
    }
    setTimeout(function () { URL.revokeObjectURL(url); }, 60000);
    toast('ok', t('c.downloaded'), 4000);
  }

  // 32 random bytes as 43 characters of base64url: letters, digits, "-" and "_".
  function randomKey() {
    if (!window.crypto || !window.crypto.getRandomValues) return '';
    const bytes = new Uint8Array(32);
    window.crypto.getRandomValues(bytes);
    let bin = '';
    bytes.forEach(function (b) { bin += String.fromCharCode(b); });
    return window.btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function openKeyDialog() {
    state.key.busy = false;
    const key = randomKey();
    $('key-new').value = key;
    $('key-random').disabled = key === '';
    formError('key-error', key === '' ? t('c.noCrypto') : '');
    paintKeyDialog();
    $('dlg-key').showModal();
    $('key-new').focus();
  }

  function paintKeyDialog() {
    const busy = state.key.busy;
    $('key-confirm').textContent = t(busy ? 'c.regenWorking' : 'c.regenConfirm');
    $('key-confirm').disabled = busy;
    $('key-new').disabled = busy;
  }

  async function submitKey(ev) {
    ev.preventDefault();
    const K = state.key;
    if (K.busy) return;
    const key = $('key-new').value.trim();
    const problem = !AUTH_KEY_RE.test(key) ? 'c.keyBad' : key === authKey() ? 'c.keySame' : '';
    if (problem) {
      formError('key-error', t(problem));
      $('key-new').focus();
      return;
    }
    const epoch = state.epoch;
    K.busy = true;
    formError('key-error', '');
    paintKeyDialog();
    let res;
    try {
      res = await api('PUT', 'settings', { auth_key: key });
    } catch (e) {
      if (epoch !== state.epoch) return;
      K.busy = false;
      paintKeyDialog();
      formError('key-error', e instanceof ApiError && e.code === 'invalid_settings'
        ? t('s.rejected', { msg: e.serverMessage || t('s.noReason') }) : describeError(e));
      return;
    }
    if (epoch !== state.epoch) return;
    K.busy = false;
    $('key-new').value = '';
    $('dlg-key').close();
    await takeSettings(res, null);
    if (epoch === state.epoch) toast('ok', t('c.regenDone'), 9000);
  }

  // ---- storage test (docs/API.md 3.8)

  async function runStorageTest() {
    const s = state.settings;
    if (!s.loaded || s.testing || s.saving !== null) return;
    const bad = FIELDS.filter(function (f) {
      return f.key.indexOf('storage.s3.') === 0 && fieldKnown(f) && fieldActive(f) && fieldProblem(f) !== '';
    });
    if (bad.length > 0) {
      s.tried.storage = true;
      renderSettings();
      focusField(bad[0]);
      return;
    }
    const storage = storageFromForm();
    const asked = JSON.stringify(storage);
    const epoch = state.epoch;
    s.testing = true;
    s.test = null;
    renderSettings();
    let res = null;
    let error = null;
    try {
      res = await api('POST', 'storage/test', { storage: storage }, SLOW_TIMEOUT_MS);
    } catch (e) {
      error = e;
    }
    if (epoch !== state.epoch) return;
    s.testing = false;
    // An answer about settings that were edited meanwhile says nothing about what is on screen.
    if (JSON.stringify(storageFromForm()) === asked) {
      s.test = error ? { ok: false, error: error } : { ok: res.ok === true, message: str(res.message) };
    }
    renderSettings();
  }

  // ---- admin password (docs/API.md 3.7)

  const PW_PARTS = ['current', 'new', 'repeat'];

  function paintPassword() {
    const P = state.pw;
    PW_PARTS.forEach(function (k) {
      const msg = P.errors[k] ? P.errors[k]() : '';
      formError('pw-' + k + '-error', msg);
      const input = $('pw-' + k);
      input.disabled = P.busy;
      if (msg !== '') input.setAttribute('aria-invalid', 'true');
      else input.removeAttribute('aria-invalid');
    });
    formError('pw-error', P.error ? P.error() : '');
    $('pw-submit').textContent = t(P.busy ? 'pw.working' : 'pw.submit');
    $('pw-submit').disabled = P.busy;
  }

  async function submitPassword(ev) {
    ev.preventDefault();
    const P = state.pw;
    if (P.busy) return;
    const current = $('pw-current').value;
    const next = $('pw-new').value;
    const length = Array.from(next).length;
    P.errors = {};
    P.error = null;
    if (current === '') P.errors.current = function () { return t('pw.currentEmpty'); };
    if (length < PASSWORD_MIN) P.errors.new = function () { return t('pw.newShort'); };
    else if (length > PASSWORD_MAX) P.errors.new = function () { return t('pw.newLong'); };
    else if ($('pw-repeat').value !== next) P.errors.repeat = function () { return t('pw.mismatch'); };
    const firstBad = PW_PARTS.filter(function (k) { return P.errors[k]; })[0];
    if (firstBad) {
      paintPassword();
      $('pw-' + firstBad).focus();
      return;
    }

    const epoch = state.epoch;
    P.busy = true;
    paintPassword();
    try {
      await api('POST', 'password', { current: current, new: next });
    } catch (e) {
      if (epoch !== state.epoch) return;
      P.busy = false;
      const code = e instanceof ApiError ? e.code : '';
      let focus = null;
      if (code === 'bad_password') {
        P.errors.current = function () { return t('pw.currentBad'); };
        focus = 'pw-current';
      } else if (code === 'weak_password') {
        const msg = e.serverMessage;
        P.errors.new = function () { return msg ? t('pw.weak') + t('err.sep') + msg : t('pw.weak'); };
        focus = 'pw-new';
      } else if (e instanceof ApiError && e.status === 429) {
        const wait = e.data ? Math.ceil(num(e.data.retry_after)) : 0;
        P.error = function () { return wait > 0 ? t('pw.locked', { time: fmtCountdown(wait) }) : t('pw.lockedNoTime'); };
      } else {
        P.error = function () { return describeError(e); };
      }
      paintPassword();
      if (focus) $(focus).select();
      return;
    }
    if (epoch !== state.epoch) return;
    P.busy = false;
    PW_PARTS.forEach(function (k) { $('pw-' + k).value = ''; });
    paintPassword();
    toast('ok', t('pw.done'), 7000);
    loadOverview({ quiet: true }); // "initial_password" is false now: the reminder goes away
  }

  // ---- cleanup

  async function runCleanup() {
    const s = state.settings;
    if (s.cleaning) return;
    s.cleaning = true;
    s.cleanResult = null;
    paintCleanup();
    let res;
    try {
      res = await api('POST', 'cleanup');
    } catch (e) {
      s.cleaning = false;
      if (e.handled) return;
      paintCleanup();
      toast('err', describeError(e));
      return;
    }
    s.cleaning = false;
    s.cleanResult = { deleted: num(res.deleted), freed: num(res.freed_bytes) };
    paintCleanup();
    toast('ok', $('s-clean-result').textContent);
    refreshAfterChange();
  }

  // ------------------------------------------------------------------- i18n

  function applyStaticTexts() {
    document.documentElement.lang = state.lang === 'zh' ? 'zh-CN' : 'en';
    document.title = t('doc.title') + ' · vml-sync-server';
    document.querySelectorAll('[data-i18n]').forEach(function (el) { el.textContent = t(el.dataset.i18n); });
    document.querySelectorAll('[data-i18n-attr]').forEach(function (el) {
      el.dataset.i18nAttr.split(';').forEach(function (pair) {
        const i = pair.indexOf(':');
        if (i > 0) el.setAttribute(pair.slice(0, i).trim(), t(pair.slice(i + 1).trim()));
      });
    });
    document.querySelectorAll('.js-lang-label').forEach(function (el) { el.textContent = t('lang.other'); });
    document.querySelectorAll('.js-lang').forEach(function (el) { el.setAttribute('lang', state.lang === 'zh' ? 'en' : 'zh-CN'); });
  }

  function renderAll() {
    applyStaticTexts();
    paintTrouble();
    paintLogin();
    paintPageSize();
    renderOverview();
    renderModels();
    renderBlocklist();
    renderUploads();
    renderSettings();
    if ($('dlg-delete').open) paintDeleteDialog();
    if ($('dlg-unblock').open) paintUnblockDialog();
    paintAddDialog();
    paintKeyDialog();
    paintConfirm();
  }

  function toggleLang() {
    state.lang = state.lang === 'zh' ? 'en' : 'zh';
    lsSet(LS_LANG, state.lang);
    renderAll();
  }

  // ------------------------------------------------------------------- wiring

  function debounce(fn, ms) {
    let timer = null;
    return function () {
      const args = arguments;
      clearTimeout(timer);
      timer = setTimeout(function () { fn.apply(null, args); }, ms);
    };
  }

  function wire() {
    $('boot-retry').addEventListener('click', boot);
    $('login-form').addEventListener('submit', submitLogin);
    $('login-password').addEventListener('input', function () {
      if (state.login.error) { state.login.error = null; paintLogin(); }
    });
    document.querySelectorAll('.js-lang').forEach(function (b) { b.addEventListener('click', toggleLang); });

    $('btn-refresh').addEventListener('click', function () { refreshAll('user'); });
    $('btn-logout').addEventListener('click', logout);
    $('conn-retry').addEventListener('click', function () { refreshAll('user'); });
    $('ov-edit-retention').addEventListener('click', function () { goToSetting('retention', 'f-retention'); });
    $('ov-edit-storage').addEventListener('click', function () { goToSetting('storage'); });
    $('pw-banner-go').addEventListener('click', function () { goToSetting('password', 'pw-current'); });

    // tabs
    TABS.forEach(function (name, i) {
      const btn = $('tab-' + name);
      btn.addEventListener('click', function () { openTab(name); });
      btn.addEventListener('keydown', function (ev) {
        let j = -1;
        if (ev.key === 'ArrowRight') j = (i + 1) % TABS.length;
        else if (ev.key === 'ArrowLeft') j = (i + TABS.length - 1) % TABS.length;
        else if (ev.key === 'Home') j = 0;
        else if (ev.key === 'End') j = TABS.length - 1;
        if (j < 0) return;
        ev.preventDefault();
        openTab(TABS[j]);
        $('tab-' + TABS[j]).focus();
      });
    });
    window.addEventListener('hashchange', function () {
      if (state.view === 'app' && tabFromHash() !== state.tab) openTab(tabFromHash());
    });

    // models
    const onSearch = debounce(function () { applySearch($('m-search').value.trim()); }, 300);
    $('m-search').addEventListener('input', onSearch);
    $('m-search').addEventListener('keydown', function (ev) {
      if (ev.key === 'Enter') { ev.preventDefault(); applySearch($('m-search').value.trim()); }
    });
    document.querySelectorAll('#panel-models th[data-sort] .th-sort').forEach(function (b) {
      b.addEventListener('click', function () { setSort(b.closest('th').dataset.sort); });
    });
    $('m-sort').addEventListener('change', function () { setSort($('m-sort').value, state.models.order); });
    $('m-order').addEventListener('click', function () {
      setSort(state.models.sort, state.models.order === 'asc' ? 'desc' : 'asc');
    });
    $('m-limit').addEventListener('change', function () {
      const n = Number($('m-limit').value);
      if (PAGE_SIZES.indexOf(n) < 0) return;
      state.models.limit = n;
      state.models.offset = 0;
      lsSet(LS_LIMIT, String(n));
      loadModels({ quiet: false });
    });
    $('m-prev').addEventListener('click', function () {
      const m = state.models;
      m.offset = Math.max(0, m.offset - m.limit);
      loadModels({ quiet: false });
    });
    $('m-next').addEventListener('click', function () {
      const m = state.models;
      if (m.offset + m.limit < m.total) { m.offset += m.limit; loadModels({ quiet: false }); }
    });
    $('m-all').addEventListener('change', function () {
      const on = $('m-all').checked;
      state.models.items.forEach(function (it) {
        if (on) state.selected.set(str(it.id), str(it.name));
        else state.selected.delete(str(it.id));
      });
      renderModels();
    });
    $('m-clear-sel').addEventListener('click', function () { state.selected.clear(); renderModels(); });
    $('m-del-selected').addEventListener('click', function () {
      openDeleteDialog(Array.from(state.selected, function (e) { return { id: e[0], name: e[1] }; }));
    });

    // blocklist
    $('b-filter').addEventListener('input', function () {
      state.blocklist.filter = $('b-filter').value;
      renderBlocklist();
    });
    $('b-add').addEventListener('click', openAddDialog);
    $('b-all').addEventListener('change', function () {
      const on = $('b-all').checked;
      blocklistVisible().forEach(function (x) {
        if (on) state.blocklist.selected.add(str(x.id));
        else state.blocklist.selected.delete(str(x.id));
      });
      renderBlocklist();
    });
    $('b-clear-sel').addEventListener('click', function () { state.blocklist.selected.clear(); renderBlocklist(); });
    $('b-unblock-selected').addEventListener('click', function () {
      const b = state.blocklist;
      openUnblockDialog(b.items.filter(function (x) { return b.selected.has(str(x.id)); })
        .map(function (x) { return { id: str(x.id), name: str(x.name) }; }));
    });

    // settings
    FIELDS.forEach(function (f) {
      // where a message about this one setting goes (client-side check or invalid_settings)
      fieldWrap(f).appendChild(h('p', { class: 'field-error', role: 'alert', hidden: true }));
    });
    GROUPS.forEach(function (group) {
      const form = $('g-' + group);
      form.addEventListener('submit', function (ev) { ev.preventDefault(); submitGroup(group); });
      form.querySelector('.js-reset').addEventListener('click', function () { resetGroup(group); });
      const edited = function (ev) {
        const wrap = ev.target.closest('[data-field]');
        if (wrap && FIELD_BY_KEY[wrap.dataset.field]) onFieldEdited(FIELD_BY_KEY[wrap.dataset.field]);
      };
      form.addEventListener('input', edited);
      form.addEventListener('change', edited);
    });
    document.querySelectorAll('#s-nav [data-goto]').forEach(function (b) {
      b.addEventListener('click', function () { goToSetting(b.dataset.goto); });
    });
    $('s-load-retry').addEventListener('click', function () { loadSettings({ quiet: false }); });
    $('s-clean').addEventListener('click', runCleanup);
    $('st-test').addEventListener('click', runStorageTest);

    $('c-url-copy').addEventListener('click', function () { copyWithToast(clientUrl(), 'c.copiedUrl'); });
    $('c-key-copy').addEventListener('click', function () { copyWithToast(authKey(), 'c.copiedKey'); });
    $('c-key-toggle').addEventListener('click', function () {
      state.settings.keyShown = !state.settings.keyShown;
      paintClient();
    });
    $('c-json-copy').addEventListener('click', function () { copyWithToast(syncJson(authKey()), 'c.copiedJson'); });
    $('c-json-download').addEventListener('click', downloadSyncFile);
    $('c-regen').addEventListener('click', openKeyDialog);
    $('key-form').addEventListener('submit', submitKey);
    $('key-new').addEventListener('input', function () { formError('key-error', ''); });
    $('key-random').addEventListener('click', function () {
      $('key-new').value = randomKey();
      formError('key-error', '');
    });

    $('g-password').addEventListener('submit', submitPassword);
    PW_PARTS.forEach(function (k) {
      $('pw-' + k).addEventListener('input', function () {
        if (!state.pw.errors[k] && !state.pw.error) return;
        delete state.pw.errors[k];
        state.pw.error = null;
        paintPassword();
      });
    });

    // Closing or reloading the page with settings changed but not saved: ask first.
    window.addEventListener('beforeunload', function (ev) {
      if (state.view !== 'app' || state.leaving || !anyDirty()) return;
      ev.preventDefault();
      ev.returnValue = '';
    });

    // dialogs
    $('cf-form').addEventListener('submit', submitConfirm);
    $('del-form').addEventListener('submit', submitDelete);
    $('del-block').addEventListener('change', function () {
      paintDeleteDialog();
      if ($('del-block').checked) $('del-note').focus();
    });
    $('unb-form').addEventListener('submit', submitUnblock);
    $('add-form').addEventListener('submit', submitAdd);
    $('add-id').addEventListener('input', function () { formError('add-error', ''); });
    document.querySelectorAll('dialog').forEach(function (dlg) {
      dlg.querySelectorAll('.js-dlg-cancel').forEach(function (b) {
        b.addEventListener('click', function () { if (!dialogBusy()) dlg.close(); });
      });
      // Esc must not close a dialog while its request is on the way.
      dlg.addEventListener('cancel', function (ev) { if (dialogBusy()) ev.preventDefault(); });
      // A click on the dimmed area around the dialog closes it.
      dlg.addEventListener('mousedown', function (ev) { if (ev.target === dlg && !dialogBusy()) dlg.close(); });
    });
    // A typed key does not stay in a closed dialog.
    $('dlg-key').addEventListener('close', function () { $('key-new').value = ''; });

    document.addEventListener('visibilitychange', onVisibility);
    setInterval(tickTimes, 20000);
  }

  function init() {
    state.lang = detectLang();
    const savedLimit = Number(lsGet(LS_LIMIT));
    if (PAGE_SIZES.indexOf(savedLimit) >= 0) state.models.limit = savedLimit;
    pendingFlash = ssTake(SS_FLASH);
    wire();
    renderAll();
    boot();
  }

  init();
})();
