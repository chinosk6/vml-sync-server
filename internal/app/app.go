// Package app 是服务的核心逻辑：设置、上传会话、内容核对、下载计数、禁止名单、过期清理和对账。
// HTTP 层（internal/httpapi）只负责把请求翻译成这里的调用。
package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"vml-sync-server/internal/meta"
	"vml-sync-server/internal/passwd"
	"vml-sync-server/internal/s3"
	"vml-sync-server/internal/settings"
	"vml-sync-server/internal/storage"
)

// 接口里出现的各种 state。
const (
	StateReady       = "ready"
	StateUploading   = "uploading"
	StateMissing     = "missing"
	StateBlocked     = "blocked"
	StateTooLarge    = "too_large"
	StateStorageFull = "storage_full"
	StateBusy        = "busy"
	StateUpload      = "upload"
	StateIncomplete  = "incomplete"
	StateRejected    = "rejected"
)

// 上传被拒收的原因。
const (
	ReasonSizeMismatch = "size_mismatch"
	ReasonHashMismatch = "hash_mismatch"
	ReasonNotABundle   = "not_a_bundle"
)

// 文件名、昵称、备注的长度上限（按字符算）。
const (
	MaxNameChars     = 200
	MaxUploaderChars = 64
	MaxNoteChars     = 500
)

// UnknownName 是对账时补记录用的文件名。
const UnknownName = "(unknown)"

// InitialPasswordFile 是初始密码文件在数据目录里的名字。
const InitialPasswordFile = "initial-admin-password.txt"

// busyRetryAfter 是返回 busy 时建议客户端等多少秒再试。
const busyRetryAfter = 10

// finishedTTL 是已结束的上传会话的结果保留多久，供超时重试的 complete 请求取回。
const finishedTTL = 10 * time.Minute

// bundleMagic 是 Unity 资源包文件开头的 7 个字节。
var bundleMagic = []byte("UnityFS")

// Error 是要原样返回给调用方的接口错误（HTTP 状态码 + 错误代码）。
type Error struct {
	Status  int
	Code    string
	Message string
	// Field 只在 invalid_settings 里有：出问题的设置项。
	Field string
	// ModelCount 只在 storage_change_needs_confirm 里有：现有的模型数。
	ModelCount *int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// 接口会用到的错误。
var (
	ErrUploadGone         = &Error{Status: 404, Code: "upload_gone", Message: "upload session does not exist or has timed out"}
	ErrBadPart            = &Error{Status: 400, Code: "bad_part", Message: "part number out of range or wrong part length"}
	ErrWrongMode          = &Error{Status: 409, Code: "wrong_mode", Message: "this upload session uses the other transfer mode"}
	ErrNotFound           = &Error{Status: 404, Code: "not_found", Message: "not found"}
	ErrPartTooLarge       = &Error{Status: 413, Code: "too_large", Message: "part is larger than this upload's chunk size"}
	ErrStorageUnavailable = &Error{Status: 503, Code: "storage_unavailable", Message: "the configured storage is not usable right now; the administrator has to check the storage settings"}
)

// Options 是创建 App 时的参数。只有 DataDir 是必填的。
type Options struct {
	// DataDir 是数据目录：设置、元数据、上传中的临时文件、本地存储的模型文件都在里面。
	DataDir string
	// Now 返回当前时间；留空用 time.Now。测试里用它拨动时间。
	Now func() time.Time

	// 上传会话的空闲超时：中转按“有没有字节在流入”算，直连按最后一次接口调用算。
	ProxyIdleTimeout  time.Duration
	DirectIdleTimeout time.Duration
	// 过期清理、会话清理、存储不可用时重试的间隔。
	CleanupInterval      time.Duration
	SweepInterval        time.Duration
	StorageRetryInterval time.Duration

	// BytesPerMB 是设置里的“1 MB”换算成多少字节；留空是 1048576。
	// 只给测试用：调小之后可以拿很小的文件测分片逻辑。
	BytesPerMB int64
	// PasswordIterations 是密码哈希的迭代次数；留空用 passwd.DefaultIterations。测试里调小省时间。
	PasswordIterations int
}

func (o *Options) fillDefaults() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ProxyIdleTimeout <= 0 {
		o.ProxyIdleTimeout = 120 * time.Second
	}
	if o.DirectIdleTimeout <= 0 {
		o.DirectIdleTimeout = 900 * time.Second
	}
	if o.CleanupInterval <= 0 {
		o.CleanupInterval = 10 * time.Minute
	}
	if o.SweepInterval <= 0 {
		o.SweepInterval = 5 * time.Second
	}
	if o.StorageRetryInterval <= 0 {
		o.StorageRetryInterval = 30 * time.Second
	}
	if o.BytesPerMB <= 0 {
		o.BytesPerMB = 1 << 20
	}
	if o.PasswordIterations <= 0 {
		o.PasswordIterations = passwd.DefaultIterations
	}
}

// state 是某一时刻的全部运行时设置。它是不可变的：改设置就是换一个新的 state。
// 每个请求开头取一次，之后用的都是同一份，不会看到改到一半的设置。
type state struct {
	set settings.Settings
	// gen 每换一次存储位置加一。上传会话记着自己开始时的 gen：
	// 存储换过之后，旧会话传完的东西不能登记到新存储名下。
	gen uint64
	// store 是按当前设置建出来的存储；建不出来（设置有问题）时为 nil。
	store  storage.Store
	direct storage.Direct // store 不支持直连时为 nil
	// ready 表示这个存储已经对过账、可以对客户端开放了。
	ready  bool
	reason string // 不可用的原因（给日志看）

	maxFileBytes  int64
	maxTotalBytes int64
	chunkBytes    int64
}

// App 持有全部运行时状态。所有导出方法都可以并发调用。
type App struct {
	opts Options
	db   *meta.DB
	now  func() time.Time

	dataDir   string
	uploadDir string // 中转上传的临时文件放在这里

	st atomic.Pointer[state]
	// setMu 让“改设置”和“启用存储”一个一个来。
	setMu sync.Mutex

	// mu 保护上传会话表。持有它的时候不做网络和大块磁盘读写。
	// 换存储时“作废全部会话、清空模型记录、换上新 state”也在这把锁里一次做完。
	mu       sync.RWMutex
	uploads  map[string]*upload  // 上传会话 id → 会话
	byModel  map[string]*upload  // 模型 id → 会话
	finished map[string]finished // 刚结束的会话 id → 结果

	// idLocks 按模型 id 分组的锁：同一个模型的“放进存储 + 登记”、“删除”、“记一次下载”
	// 互相排队，免得删除和登记交错出“有记录没文件”这类状态。
	idLocks [256]sync.Mutex

	cleanMu     sync.Mutex // 同一时间只跑一轮过期清理
	lastCleanup atomic.Int64
	nextCleanup atomic.Int64

	// hashSlots 限制同时进行的密码哈希计算数（每次都很费 CPU）。
	hashSlots chan struct{}

	// firstRunPassword 是这次启动时新生成的初始密码；不是首次启动时为空。
	firstRunPassword string
}

type finished struct {
	result  CompleteResult
	expires time.Time
}

// New 创建 App：数据目录里还没有设置时做首次启动的初始化（生成客户端密钥和管理员初始密码）。
// 调用方接着应该调 Activate，然后在后台跑 Run。
func New(db *meta.DB, opts Options) (*App, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, fmt.Errorf("data directory is not set")
	}
	opts.fillDefaults()
	a := &App{
		opts:      opts,
		db:        db,
		now:       opts.Now,
		dataDir:   opts.DataDir,
		uploadDir: filepath.Join(opts.DataDir, "uploads"),
		uploads:   make(map[string]*upload),
		byModel:   make(map[string]*upload),
		finished:  make(map[string]finished),
		hashSlots: make(chan struct{}, 4),
	}
	if err := os.MkdirAll(a.uploadDir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create upload directory: %w", err)
	}
	// 上传会话只存在内存里，临时目录里的东西都是上次运行留下的。
	if entries, err := os.ReadDir(a.uploadDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				os.Remove(filepath.Join(a.uploadDir, e.Name()))
			}
		}
	}

	set, err := loadOrInitSettings(db)
	if err != nil {
		return nil, err
	}
	password, err := ensureAdminPassword(db, a.dataDir, opts.PasswordIterations)
	if err != nil {
		return nil, err
	}
	a.firstRunPassword = password

	st := a.newState(set, 1)
	st.setStore(a.openStore(set.Storage))
	a.st.Store(st)
	return a, nil
}

// newState 按设置算出一个新的 state（存储相关的字段由调用方填）。
func (a *App) newState(set settings.Settings, gen uint64) *state {
	return &state{
		set:           set,
		gen:           gen,
		maxFileBytes:  set.MaxFileMB * a.opts.BytesPerMB,
		maxTotalBytes: set.MaxTotalMB * a.opts.BytesPerMB,
		chunkBytes:    set.ChunkMB * a.opts.BytesPerMB,
	}
}

// setStore 填上存储相关的字段。store 为 nil 时 reason 说明为什么建不出来。
func (st *state) setStore(store storage.Store, reason string) {
	st.store, st.direct = store, nil
	if d, ok := store.(storage.Direct); ok {
		st.direct = d
	}
	st.reason = reason
}

// openStore 按存储设置建出存储。建不出来时返回 nil 和原因。
func (a *App) openStore(c settings.Storage) (storage.Store, string) {
	switch c.Type {
	case settings.StorageS3:
		client, err := s3.New(s3.Config{
			Endpoint:        c.S3.Endpoint,
			Region:          c.S3.Region,
			Bucket:          c.S3.Bucket,
			AccessKeyID:     c.S3.AccessKeyID,
			SecretAccessKey: c.S3.SecretAccessKey,
			PathStyle:       c.S3.PathStyle,
		})
		if err != nil {
			return nil, err.Error()
		}
		return storage.NewS3(client, c.S3.Prefix), ""
	default:
		local, err := storage.NewLocal(filepath.Join(a.dataDir, "objects"))
		if err != nil {
			return nil, err.Error()
		}
		return local, ""
	}
}

// Settings 返回当前的设置。
func (a *App) Settings() settings.Settings { return a.st.Load().set }

// DataDir 返回数据目录。
func (a *App) DataDir() string { return a.dataDir }

// ProxyIdleTimeout 返回中转上传的空闲超时（HTTP 层用它给分片请求设读超时）。
func (a *App) ProxyIdleTimeout() time.Duration { return a.opts.ProxyIdleTimeout }

// StorageReady 报告存储现在能不能用；不能用时附带原因。
func (a *App) StorageReady() (bool, string) {
	st := a.st.Load()
	return st.ready, st.reason
}

// FirstRunPassword 返回这次启动时新生成的管理员初始密码（给启动日志打印一次）；不是首次启动时为空。
func (a *App) FirstRunPassword() string { return a.firstRunPassword }

// current 返回当前的 state；存储还不能用时返回 ErrStorageUnavailable。
func (a *App) current() (*state, error) {
	st := a.st.Load()
	if !st.ready {
		return nil, ErrStorageUnavailable
	}
	return st, nil
}

// Close 结束所有还在进行的上传会话并清掉它们的临时数据（含对象存储上没完成的分片上传）。
func (a *App) Close() {
	a.mu.RLock()
	all := make([]*upload, 0, len(a.uploads))
	for _, u := range a.uploads {
		all = append(all, u)
	}
	a.mu.RUnlock()
	for _, u := range all {
		a.drop(u, true)
	}
}

// lockID 锁住一个模型 id，返回解锁函数。id 必须已经校验过格式。
func (a *App) lockID(id string) func() {
	m := &a.idLocks[hexVal(id[0])<<4|hexVal(id[1])]
	m.Lock()
	return m.Unlock
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return 0
}

// internalError 把内部错误记进日志，返回给调用方一个不带细节的错误。
func internalError(op string, err error) error {
	log.Printf("error: %s: %v", op, err)
	return &Error{Status: 500, Code: "internal", Message: "internal server error"}
}

// CleanText 整理别人填的文字（文件名、昵称、备注）：去掉控制字符和会打乱显示方向的字符，
// 去掉首尾空白，截到最多 max 个字符。
func CleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || isDisplayControl(r) {
			return -1
		}
		return r
	}, s))
	n := 0
	for i := range s {
		if n == max {
			s = s[:i]
			break
		}
		n++
	}
	return strings.TrimSpace(s)
}

// isDisplayControl 报告 r 是不是换行 / 换段分隔符或者双向文字控制符
// （后者能让文件名在页面上倒着显示，常被用来伪装扩展名）。
func isDisplayControl(r rune) bool {
	switch {
	case r == 0x2028 || r == 0x2029:
		return true
	case r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x200e || r == 0x200f || r == 0xfeff:
		return true
	}
	return false
}

func ptr[T any](v T) *T { return &v }

// backgroundContext 给不该被某个请求的断开打断的收尾工作用。
func backgroundContext(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
