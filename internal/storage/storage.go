// Package storage 是模型文件的存放位置：本地磁盘，或者 S3 兼容的对象存储。
package storage

import (
	"context"
	"errors"
	"io"
	"regexp"
	"time"
)

// ErrNotExist 表示对象不存在。
var ErrNotExist = errors.New("storage: object does not exist")

// 存储类型的名字（管理页的概况里会显示）。
const (
	KindLocal = "local"
	KindS3    = "s3"
)

var (
	modelIDPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	uploadIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ValidModelID 报告 s 是不是合法的模型 id。id 只有通过这个检查才允许参与拼路径 / 对象名。
func ValidModelID(s string) bool { return modelIDPattern.MatchString(s) }

// ValidUploadID 报告 s 是不是合法的上传会话 id。
func ValidUploadID(s string) bool { return uploadIDPattern.MatchString(s) }

var errBadID = errors.New("storage: invalid id")

// Object 是存储里的一个模型文件。
type Object struct {
	ID      string
	Size    int64
	ModTime time.Time
}

// Store 是存放模型文件的地方。所有方法都可以并发调用。
//
// 约定：最终位置（按模型 id 能取到的位置）上只会出现已经通过核对的内容。
type Store interface {
	// Kind 返回 "local" 或 "s3"。
	Kind() string
	// PutFile 把一个已经核对过的本地文件存成 id 对应的对象。sha256 是它的 SHA-256。
	// 成功后 path 上的文件可能已经被移走；失败时它还在原处。
	PutFile(ctx context.Context, id, path string, size int64, sha256 []byte) error
	// Open 读取对象从 offset 开始的 length 个字节；length < 0 表示读到结尾。
	// 对象不存在时返回 ErrNotExist。
	Open(ctx context.Context, id string, offset, length int64) (io.ReadCloser, error)
	// Stat 返回对象的大小和修改时间；不存在时返回 ErrNotExist。
	Stat(ctx context.Context, id string) (Object, error)
	// Delete 删除对象；对象不存在不算错。
	Delete(ctx context.Context, id string) error
	// List 列出全部对象（名字不是合法 id 的东西一律跳过）。
	List(ctx context.Context) ([]Object, error)
	// CleanupStale 清掉上次运行留下的临时物（半截的临时文件、没完成的分片上传等）。
	// 只在这个存储刚被启用、还没有任何上传会话的时候调用。
	CleanupStale(ctx context.Context) error
	// Probe 验证这个存储真的能用：写一个探测对象、读回来、删掉。不碰任何模型。
	Probe(ctx context.Context) error
}

// Part 是直连上传里已经传到对象存储的一片。
type Part struct {
	Number int
	ETag   string
	Size   int64
}

// Direct 是“客户端凭临时地址直连对象存储”需要的额外能力，只有 S3 存储实现。
//
// 直连上传的内容先落在一个暂存位置（按上传会话 id 命名），核对通过后才由 Promote
// 复制到最终位置。这样最终位置上任何时候都不会出现没核对过的内容。
type Direct interface {
	// Begin 为一个上传会话开始分片上传，返回对象存储那边的句柄。
	Begin(ctx context.Context, uploadID string) (handle string, err error)
	// PartURL 生成上传第 part 片用的临时地址。
	PartURL(uploadID, handle string, part int, ttl time.Duration) (string, error)
	// Parts 列出已经传到的分片（按片号升序）。
	Parts(ctx context.Context, uploadID, handle string) ([]Part, error)
	// Assemble 把分片拼成暂存对象。parts 必须按片号升序。
	Assemble(ctx context.Context, uploadID, handle string, parts []Part) error
	// StatStaged 返回暂存对象的大小。
	StatStaged(ctx context.Context, uploadID string) (int64, error)
	// OpenStaged 读取暂存对象从 offset 开始的 length 个字节；length < 0 表示读到结尾。
	OpenStaged(ctx context.Context, uploadID string, offset, length int64) (io.ReadCloser, error)
	// Promote 把核对通过的暂存对象变成 id 对应的最终对象，并删掉暂存对象。
	Promote(ctx context.Context, uploadID, id string) error
	// DiscardStaged 删掉暂存对象；不存在不算错。
	DiscardStaged(ctx context.Context, uploadID string) error
	// Abort 放弃还没拼起来的分片上传；不存在不算错。
	Abort(ctx context.Context, uploadID, handle string) error
	// DownloadURL 生成下载 id 对应对象用的临时地址。
	DownloadURL(id string, ttl time.Duration) (string, error)
}
