// Package settings 定义在管理页里修改的全部设置（docs/API.md 3.5）：默认值、取值范围的检查、局部更新。
// 设置存在数据目录的元数据库里；这个包不关心它们存在哪。
package settings

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// 传输方式和存储类型的取值。
const (
	TransferProxy  = "proxy"
	TransferDirect = "direct"

	StorageLocal = "local"
	StorageS3    = "s3"
)

// 各项的取值范围。
const (
	MaxRetentionHours   = 87600
	MaxBasePathLen      = 64
	MinAuthKeyLen       = 16
	MaxAuthKeyLen       = 128
	MaxFileMBLimit      = 5000
	MaxTotalMBLimit     = 1 << 40 // 只为了不让换算成字节时溢出
	MaxChunkMB          = 64
	MinDirectChunkMB    = 5 // S3 分片上传要求除最后一片外每片不小于 5 MB
	MaxActiveUploadsMax = 64
	MinDirectURLTTL     = 60
	MaxDirectURLTTL     = 86400
	maxS3FieldLen       = 1024
)

// Settings 是全部设置，也是它们在数据目录里的保存形式。
// 注意里面有 secret_access_key：对外输出一律用 View()，不要直接把它编码出去。
type Settings struct {
	RetentionHours        int     `json:"retention_hours"`
	BasePath              string  `json:"base_path"`
	AuthKey               string  `json:"auth_key"`
	MaxFileMB             int64   `json:"max_file_mb"`
	MaxTotalMB            int64   `json:"max_total_mb"`
	ChunkMB               int64   `json:"chunk_mb"`
	MaxActiveUploads      int     `json:"max_active_uploads"`
	Transfer              string  `json:"transfer"`
	DirectURLTTLSeconds   int     `json:"direct_url_ttl_seconds"`
	VerifyDirectUploads   bool    `json:"verify_direct_uploads"`
	TrustForwardedHeaders bool    `json:"trust_forwarded_headers"`
	Storage               Storage `json:"storage"`
}

// Storage 是“文件存在哪”的设置。
type Storage struct {
	Type string `json:"type"`
	S3   S3     `json:"s3"`
}

// S3 是 S3 兼容对象存储的设置。
type S3 struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Prefix          string `json:"prefix"`
	PathStyle       bool   `json:"path_style"`
}

// Default 返回默认设置（auth_key 留空，由首次启动时生成）。
func Default() Settings {
	return Settings{
		RetentionHours:      72,
		MaxFileMB:           200,
		ChunkMB:             5,
		MaxActiveUploads:    8,
		Transfer:            TransferProxy,
		DirectURLTTLSeconds: 900,
		VerifyDirectUploads: true,
		Storage: Storage{
			Type: StorageLocal,
			S3:   S3{Region: "auto", Prefix: "models/", PathStyle: true},
		},
	}
}

// NewAuthKey 随机生成一个客户端密钥。
func NewAuthKey() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// View 是设置对外输出的形式：和 Settings 一样，只是永远不带 secret_access_key，
// 只用 secret_access_key_set 说明有没有设过。
type View struct {
	RetentionHours        int         `json:"retention_hours"`
	BasePath              string      `json:"base_path"`
	AuthKey               string      `json:"auth_key"`
	MaxFileMB             int64       `json:"max_file_mb"`
	MaxTotalMB            int64       `json:"max_total_mb"`
	ChunkMB               int64       `json:"chunk_mb"`
	MaxActiveUploads      int         `json:"max_active_uploads"`
	Transfer              string      `json:"transfer"`
	DirectURLTTLSeconds   int         `json:"direct_url_ttl_seconds"`
	VerifyDirectUploads   bool        `json:"verify_direct_uploads"`
	TrustForwardedHeaders bool        `json:"trust_forwarded_headers"`
	Storage               StorageView `json:"storage"`
}

// StorageView 是 Storage 对外输出的形式。
type StorageView struct {
	Type string `json:"type"`
	S3   S3View `json:"s3"`
}

// S3View 是 S3 对外输出的形式。
type S3View struct {
	Endpoint           string `json:"endpoint"`
	Region             string `json:"region"`
	Bucket             string `json:"bucket"`
	AccessKeyID        string `json:"access_key_id"`
	SecretAccessKeySet bool   `json:"secret_access_key_set"`
	Prefix             string `json:"prefix"`
	PathStyle          bool   `json:"path_style"`
}

// View 返回可以对外输出的形式。
func (s Settings) View() View {
	return View{
		RetentionHours:        s.RetentionHours,
		BasePath:              s.BasePath,
		AuthKey:               s.AuthKey,
		MaxFileMB:             s.MaxFileMB,
		MaxTotalMB:            s.MaxTotalMB,
		ChunkMB:               s.ChunkMB,
		MaxActiveUploads:      s.MaxActiveUploads,
		Transfer:              s.Transfer,
		DirectURLTTLSeconds:   s.DirectURLTTLSeconds,
		VerifyDirectUploads:   s.VerifyDirectUploads,
		TrustForwardedHeaders: s.TrustForwardedHeaders,
		Storage: StorageView{
			Type: s.Storage.Type,
			S3: S3View{
				Endpoint:           s.Storage.S3.Endpoint,
				Region:             s.Storage.S3.Region,
				Bucket:             s.Storage.S3.Bucket,
				AccessKeyID:        s.Storage.S3.AccessKeyID,
				SecretAccessKeySet: s.Storage.S3.SecretAccessKey != "",
				Prefix:             s.Storage.S3.Prefix,
				PathStyle:          s.Storage.S3.PathStyle,
			},
		},
	}
}

// Patch 是一次局部更新：只改不为 nil 的那些项，其余保持原样。
type Patch struct {
	RetentionHours        *int          `json:"retention_hours"`
	BasePath              *string       `json:"base_path"`
	AuthKey               *string       `json:"auth_key"`
	MaxFileMB             *int64        `json:"max_file_mb"`
	MaxTotalMB            *int64        `json:"max_total_mb"`
	ChunkMB               *int64        `json:"chunk_mb"`
	MaxActiveUploads      *int          `json:"max_active_uploads"`
	Transfer              *string       `json:"transfer"`
	DirectURLTTLSeconds   *int          `json:"direct_url_ttl_seconds"`
	VerifyDirectUploads   *bool         `json:"verify_direct_uploads"`
	TrustForwardedHeaders *bool         `json:"trust_forwarded_headers"`
	Storage               *StoragePatch `json:"storage"`

	// ConfirmStorageChange 不是设置项：换存储位置等于清空，必须带上它才执行。
	ConfirmStorageChange bool `json:"confirm_storage_change"`
}

// StoragePatch 是对 Storage 的局部更新。
type StoragePatch struct {
	Type *string  `json:"type"`
	S3   *S3Patch `json:"s3"`
}

// S3Patch 是对 S3 的局部更新。SecretAccessKey 为 nil 或空串都表示保持原来的。
type S3Patch struct {
	Endpoint        *string `json:"endpoint"`
	Region          *string `json:"region"`
	Bucket          *string `json:"bucket"`
	AccessKeyID     *string `json:"access_key_id"`
	SecretAccessKey *string `json:"secret_access_key"`
	Prefix          *string `json:"prefix"`
	PathStyle       *bool   `json:"path_style"`
}

// Apply 把局部更新套到 s 的一份拷贝上并返回。结果还没有检查过，调用方接着要调 Validate。
func (p Patch) Apply(s Settings) Settings {
	set(&s.RetentionHours, p.RetentionHours)
	if p.BasePath != nil {
		s.BasePath = strings.TrimSpace(*p.BasePath)
	}
	if p.AuthKey != nil {
		s.AuthKey = strings.TrimSpace(*p.AuthKey)
	}
	set(&s.MaxFileMB, p.MaxFileMB)
	set(&s.MaxTotalMB, p.MaxTotalMB)
	set(&s.ChunkMB, p.ChunkMB)
	set(&s.MaxActiveUploads, p.MaxActiveUploads)
	set(&s.Transfer, p.Transfer)
	set(&s.DirectURLTTLSeconds, p.DirectURLTTLSeconds)
	set(&s.VerifyDirectUploads, p.VerifyDirectUploads)
	set(&s.TrustForwardedHeaders, p.TrustForwardedHeaders)
	s.Storage = p.Storage.Apply(s.Storage)
	return s
}

// Apply 把局部更新套到 s 的一份拷贝上并返回。p 为 nil 时原样返回。
func (p *StoragePatch) Apply(s Storage) Storage {
	if p == nil {
		return s
	}
	set(&s.Type, p.Type)
	if q := p.S3; q != nil {
		if q.Endpoint != nil {
			s.S3.Endpoint = strings.TrimRight(strings.TrimSpace(*q.Endpoint), "/")
		}
		if q.Region != nil {
			s.S3.Region = strings.TrimSpace(*q.Region)
			if s.S3.Region == "" {
				s.S3.Region = "auto"
			}
		}
		if q.Bucket != nil {
			s.S3.Bucket = strings.TrimSpace(*q.Bucket)
		}
		if q.AccessKeyID != nil {
			s.S3.AccessKeyID = strings.TrimSpace(*q.AccessKeyID)
		}
		// 只写不读：不带或给空串 = 保持原来的。
		if q.SecretAccessKey != nil && strings.TrimSpace(*q.SecretAccessKey) != "" {
			s.S3.SecretAccessKey = strings.TrimSpace(*q.SecretAccessKey)
		}
		set(&s.S3.Prefix, q.Prefix)
		set(&s.S3.PathStyle, q.PathStyle)
	}
	return s
}

func set[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// Keys 返回这次更新带上了哪些键（用文档里的写法），给日志用。不含任何值。
func (p Patch) Keys() []string {
	var keys []string
	add := func(present bool, key string) {
		if present {
			keys = append(keys, key)
		}
	}
	add(p.RetentionHours != nil, "retention_hours")
	add(p.BasePath != nil, "base_path")
	add(p.AuthKey != nil, "auth_key")
	add(p.MaxFileMB != nil, "max_file_mb")
	add(p.MaxTotalMB != nil, "max_total_mb")
	add(p.ChunkMB != nil, "chunk_mb")
	add(p.MaxActiveUploads != nil, "max_active_uploads")
	add(p.Transfer != nil, "transfer")
	add(p.DirectURLTTLSeconds != nil, "direct_url_ttl_seconds")
	add(p.VerifyDirectUploads != nil, "verify_direct_uploads")
	add(p.TrustForwardedHeaders != nil, "trust_forwarded_headers")
	if s := p.Storage; s != nil {
		add(s.Type != nil, "storage.type")
		if q := s.S3; q != nil {
			add(q.Endpoint != nil, "storage.s3.endpoint")
			add(q.Region != nil, "storage.s3.region")
			add(q.Bucket != nil, "storage.s3.bucket")
			add(q.AccessKeyID != nil, "storage.s3.access_key_id")
			add(q.SecretAccessKey != nil && *q.SecretAccessKey != "", "storage.s3.secret_access_key")
			add(q.Prefix != nil, "storage.s3.prefix")
			add(q.PathStyle != nil, "storage.s3.path_style")
		}
	}
	return keys
}

// FieldError 说明哪一项设置不合法。Field 用文档 3.5 表里的写法，例如 "storage.s3.bucket"。
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

func invalid(field, format string, args ...any) *FieldError {
	return &FieldError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Validate 检查全部设置。返回 nil 表示都合法。
func (s Settings) Validate() *FieldError {
	if s.RetentionHours < 0 || s.RetentionHours > MaxRetentionHours {
		return invalid("retention_hours", "must be an integer between 0 and %d (0 = never delete)", MaxRetentionHours)
	}
	if err := checkBasePath(s.BasePath); err != nil {
		return err
	}
	if n := len(s.AuthKey); n < MinAuthKeyLen || n > MaxAuthKeyLen {
		return invalid("auth_key", "must be %d to %d characters long", MinAuthKeyLen, MaxAuthKeyLen)
	}
	for _, r := range s.AuthKey {
		// 要放进请求头：只允许可见的 ASCII 字符。
		if r < 0x21 || r > 0x7e {
			return invalid("auth_key", "may only contain visible ASCII characters (no spaces)")
		}
	}
	if s.MaxFileMB < 1 || s.MaxFileMB > MaxFileMBLimit {
		return invalid("max_file_mb", "must be between 1 and %d", MaxFileMBLimit)
	}
	if s.MaxTotalMB < 0 || s.MaxTotalMB > MaxTotalMBLimit {
		return invalid("max_total_mb", "must not be negative (0 = unlimited)")
	}
	if s.ChunkMB < 1 || s.ChunkMB > MaxChunkMB {
		return invalid("chunk_mb", "must be between 1 and %d", MaxChunkMB)
	}
	if s.MaxActiveUploads < 1 || s.MaxActiveUploads > MaxActiveUploadsMax {
		return invalid("max_active_uploads", "must be between 1 and %d", MaxActiveUploadsMax)
	}
	if s.DirectURLTTLSeconds < MinDirectURLTTL || s.DirectURLTTLSeconds > MaxDirectURLTTL {
		return invalid("direct_url_ttl_seconds", "must be between %d and %d", MinDirectURLTTL, MaxDirectURLTTL)
	}
	if err := s.Storage.Validate(); err != nil {
		return err
	}
	switch s.Transfer {
	case TransferProxy:
	case TransferDirect:
		if s.Storage.Type != StorageS3 {
			return invalid("transfer", "direct needs storage.type to be s3: clients cannot connect directly to files on the server's local disk")
		}
		if s.ChunkMB < MinDirectChunkMB {
			return invalid("chunk_mb", "must be at least %d when transfer is direct (S3 multipart uploads need parts of 5 MB or more)", MinDirectChunkMB)
		}
	default:
		return invalid("transfer", "must be %q or %q", TransferProxy, TransferDirect)
	}
	return nil
}

// Validate 检查存储设置。
func (st Storage) Validate() *FieldError {
	c := st.S3
	// 填了的字段，不管现在用不用 S3，格式都要对。
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return invalid("storage.s3.endpoint", "must look like https://host[:port]")
		}
		if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return invalid("storage.s3.endpoint", "must be just https://host[:port] without a path: put the bucket name in storage.s3.bucket")
		}
	}
	if strings.ContainsAny(c.Bucket, "/ ") {
		return invalid("storage.s3.bucket", "must be a bucket name, not a path")
	}
	if strings.HasPrefix(c.Prefix, "/") || strings.Contains(c.Prefix, "//") {
		return invalid("storage.s3.prefix", "must not start with '/' or contain '//'")
	}
	for field, value := range map[string]string{
		"storage.s3.endpoint":          c.Endpoint,
		"storage.s3.region":            c.Region,
		"storage.s3.bucket":            c.Bucket,
		"storage.s3.access_key_id":     c.AccessKeyID,
		"storage.s3.secret_access_key": c.SecretAccessKey,
		"storage.s3.prefix":            c.Prefix,
	} {
		if len(value) > maxS3FieldLen {
			return invalid(field, "is too long")
		}
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return invalid(field, "must not contain control characters")
			}
		}
	}

	switch st.Type {
	case StorageLocal:
	case StorageS3:
		if c.Endpoint == "" {
			return invalid("storage.s3.endpoint", "is required when storage.type is s3")
		}
		if c.Bucket == "" {
			return invalid("storage.s3.bucket", "is required when storage.type is s3")
		}
		if c.AccessKeyID == "" {
			return invalid("storage.s3.access_key_id", "is required when storage.type is s3")
		}
		if c.SecretAccessKey == "" {
			return invalid("storage.s3.secret_access_key", "is required when storage.type is s3 and none has been saved yet")
		}
		if c.Region == "" {
			return invalid("storage.s3.region", "must not be empty (use \"auto\" for Cloudflare R2)")
		}
	default:
		return invalid("storage.type", "must be %q or %q", StorageLocal, StorageS3)
	}
	return nil
}

// checkBasePath 检查入口路径前缀：空，或者以 "/" 开头、不以 "/" 结尾，只含字母、数字和 - _ . ~ /。
func checkBasePath(p string) *FieldError {
	if p == "" {
		return nil
	}
	if len(p) > MaxBasePathLen {
		return invalid("base_path", "must be at most %d characters long", MaxBasePathLen)
	}
	if !strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return invalid("base_path", "must be empty, or start with '/' and not end with '/'")
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return invalid("base_path", "must not contain empty, \".\" or \"..\" segments")
		}
		for _, r := range seg {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
				r == '-' || r == '_' || r == '.' || r == '~'
			if !ok {
				return invalid("base_path", "may only contain letters, digits, '-', '_', '.', '~' and '/'")
			}
		}
	}
	return nil
}

// SameLocation 报告两组存储设置指的是不是同一个存储位置。
// 位置变了就是“换存储”：现有的模型都留在旧位置，换过去之后拿不到。
func SameLocation(a, b Storage) bool {
	if a.Type != b.Type {
		return false
	}
	if a.Type != StorageS3 {
		return true
	}
	return a.S3.Endpoint == b.S3.Endpoint && a.S3.Region == b.S3.Region && a.S3.Bucket == b.S3.Bucket &&
		a.S3.Prefix == b.S3.Prefix && a.S3.PathStyle == b.S3.PathStyle
}

// SameCredentials 报告两组存储设置的访问凭据是否相同。
func SameCredentials(a, b Storage) bool {
	return a.S3.AccessKeyID == b.S3.AccessKeyID && a.S3.SecretAccessKey == b.S3.SecretAccessKey
}
