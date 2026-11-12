package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"vml-sync-server/internal/s3"
)

// stagingDir 是暂存对象所在的“子目录”（接在对象名前缀后面）。
// 名字里带下划线和斜杠，不可能和模型 id 撞名。
const stagingDir = "_incoming/"

// S3 把模型文件存在 S3 兼容的对象存储里，对象名是 前缀 + 模型 id。
type S3 struct {
	client *s3.Client
	prefix string
}

// NewS3 创建 S3 存储。prefix 是对象名前缀，例如 "models/"。
func NewS3(client *s3.Client, prefix string) *S3 {
	return &S3{client: client, prefix: prefix}
}

// Kind 实现 Store。
func (s *S3) Kind() string { return KindS3 }

// key 返回 id 对应的对象名。
func (s *S3) key(id string) (string, error) {
	if !ValidModelID(id) {
		return "", errBadID
	}
	return s.prefix + id, nil
}

// stagingKey 返回一个上传会话的暂存对象名。
func (s *S3) stagingKey(uploadID string) (string, error) {
	if !ValidUploadID(uploadID) {
		return "", errBadID
	}
	return s.prefix + stagingDir + uploadID, nil
}

func mapNotFound(err error) error {
	if s3.IsNotFound(err) {
		return ErrNotExist
	}
	return err
}

// PutFile 实现 Store：整个文件一次 PUT，内容的 SHA-256 签进请求里。
// 对象存储的 PUT 是原子的：要么整个对象出现，要么什么都没有。
func (s *S3) PutFile(ctx context.Context, id, path string, size int64, sha256 []byte) error {
	key, err := s.key(id)
	if err != nil {
		return err
	}
	sum := hex.EncodeToString(sha256)
	put := func() error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return s.client.PutObject(ctx, key, f, size, sum)
	}
	err = put()
	if err != nil && ctx.Err() == nil {
		// 连接被对方掐断之类的偶发问题：再试一次。
		var se *s3.Error
		if !errors.As(err, &se) || se.Status >= 500 {
			err = put()
		}
	}
	return err
}

// Open 实现 Store。
func (s *S3) Open(ctx context.Context, id string, offset, length int64) (io.ReadCloser, error) {
	key, err := s.key(id)
	if err != nil {
		return nil, err
	}
	rc, err := s.client.GetObject(ctx, key, offset, length)
	return rc, mapNotFound(err)
}

// Stat 实现 Store。
func (s *S3) Stat(ctx context.Context, id string) (Object, error) {
	key, err := s.key(id)
	if err != nil {
		return Object{}, err
	}
	obj, err := s.client.HeadObject(ctx, key)
	if err != nil {
		return Object{}, mapNotFound(err)
	}
	return Object{ID: id, Size: obj.Size, ModTime: obj.LastModified}, nil
}

// Delete 实现 Store。
func (s *S3) Delete(ctx context.Context, id string) error {
	key, err := s.key(id)
	if err != nil {
		return err
	}
	return s.client.DeleteObject(ctx, key)
}

// List 实现 Store。
func (s *S3) List(ctx context.Context) ([]Object, error) {
	objects, err := s.client.ListObjects(ctx, s.prefix)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, o := range objects {
		id, ok := strings.CutPrefix(o.Key, s.prefix)
		if !ok || !ValidModelID(id) {
			continue
		}
		out = append(out, Object{ID: id, Size: o.Size, ModTime: o.LastModified})
	}
	return out, nil
}

// CleanupStale 实现 Store：上传会话只存在内存里，重启后暂存位置上的一切都是没人要的东西——
// 中止没完成的分片上传，删掉暂存对象。
func (s *S3) CleanupStale(ctx context.Context) error {
	staging := s.prefix + stagingDir
	uploads, err := s.client.ListMultipartUploads(ctx, staging)
	if err != nil {
		return err
	}
	for _, u := range uploads {
		if !strings.HasPrefix(u.Key, staging) {
			continue
		}
		if err := s.client.AbortMultipartUpload(ctx, u.Key, u.UploadID); err != nil {
			return err
		}
	}
	objects, err := s.client.ListObjects(ctx, staging)
	if err != nil {
		return err
	}
	for _, o := range objects {
		if !strings.HasPrefix(o.Key, staging) {
			continue
		}
		if err := s.client.DeleteObject(ctx, o.Key); err != nil {
			return err
		}
	}
	return nil
}

// Probe 实现 Store：在前缀下写一个探测对象、读回来、删掉。
// 探测对象的名字带下划线和短横线，不可能和模型 id 撞名。
func (s *S3) Probe(ctx context.Context) error {
	want := make([]byte, 32)
	if _, err := rand.Read(want); err != nil {
		return err
	}
	key := s.prefix + "_probe-" + hex.EncodeToString(want[:8])
	sum := sha256.Sum256(want)
	if err := s.client.PutObject(ctx, key, bytes.NewReader(want), int64(len(want)), hex.EncodeToString(sum[:])); err != nil {
		return err
	}
	rc, err := s.client.GetObject(ctx, key, 0, -1)
	var got []byte
	if err == nil {
		got, err = io.ReadAll(io.LimitReader(rc, int64(len(want))+1))
		rc.Close()
	}
	// 不管读没读成，探测对象都要删掉。
	if derr := s.client.DeleteObject(ctx, key); err == nil {
		err = derr
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("the storage returned different content than was written")
	}
	return nil
}

// Begin 实现 Direct。
func (s *S3) Begin(ctx context.Context, uploadID string) (string, error) {
	key, err := s.stagingKey(uploadID)
	if err != nil {
		return "", err
	}
	return s.client.CreateMultipartUpload(ctx, key)
}

// PartURL 实现 Direct。
func (s *S3) PartURL(uploadID, handle string, part int, ttl time.Duration) (string, error) {
	key, err := s.stagingKey(uploadID)
	if err != nil {
		return "", err
	}
	return s.client.PresignUploadPart(key, handle, part, ttl), nil
}

// Parts 实现 Direct。
func (s *S3) Parts(ctx context.Context, uploadID, handle string) ([]Part, error) {
	key, err := s.stagingKey(uploadID)
	if err != nil {
		return nil, err
	}
	parts, err := s.client.ListParts(ctx, key, handle)
	if err != nil {
		return nil, mapNotFound(err)
	}
	out := make([]Part, len(parts))
	for i, p := range parts {
		out[i] = Part{Number: p.Number, ETag: p.ETag, Size: p.Size}
	}
	return out, nil
}

// Assemble 实现 Direct。
func (s *S3) Assemble(ctx context.Context, uploadID, handle string, parts []Part) error {
	key, err := s.stagingKey(uploadID)
	if err != nil {
		return err
	}
	in := make([]s3.Part, len(parts))
	for i, p := range parts {
		in[i] = s3.Part{Number: p.Number, ETag: p.ETag, Size: p.Size}
	}
	return mapNotFound(s.client.CompleteMultipartUpload(ctx, key, handle, in))
}

// StatStaged 实现 Direct。
func (s *S3) StatStaged(ctx context.Context, uploadID string) (int64, error) {
	key, err := s.stagingKey(uploadID)
	if err != nil {
		return 0, err
	}
	obj, err := s.client.HeadObject(ctx, key)
	if err != nil {
		return 0, mapNotFound(err)
	}
	return obj.Size, nil
}

// OpenStaged 实现 Direct。
func (s *S3) OpenStaged(ctx context.Context, uploadID string, offset, length int64) (io.ReadCloser, error) {
	key, err := s.stagingKey(uploadID)
	if err != nil {
		return nil, err
	}
	rc, err := s.client.GetObject(ctx, key, offset, length)
	return rc, mapNotFound(err)
}

// Promote 实现 Direct：服务端复制到最终位置，再删掉暂存对象。
func (s *S3) Promote(ctx context.Context, uploadID, id string) error {
	src, err := s.stagingKey(uploadID)
	if err != nil {
		return err
	}
	dst, err := s.key(id)
	if err != nil {
		return err
	}
	if err := s.client.CopyObject(ctx, src, dst); err != nil {
		return mapNotFound(err)
	}
	// 暂存对象删不掉不影响结果，下次启动时会被清掉。
	_ = s.client.DeleteObject(ctx, src)
	return nil
}

// DiscardStaged 实现 Direct。
func (s *S3) DiscardStaged(ctx context.Context, uploadID string) error {
	key, err := s.stagingKey(uploadID)
	if err != nil {
		return err
	}
	return s.client.DeleteObject(ctx, key)
}

// Abort 实现 Direct。
func (s *S3) Abort(ctx context.Context, uploadID, handle string) error {
	key, err := s.stagingKey(uploadID)
	if err != nil {
		return err
	}
	return s.client.AbortMultipartUpload(ctx, key, handle)
}

// DownloadURL 实现 Direct。
func (s *S3) DownloadURL(id string, ttl time.Duration) (string, error) {
	key, err := s.key(id)
	if err != nil {
		return "", err
	}
	return s.client.PresignGetObject(key, ttl), nil
}

var (
	_ Store  = (*Local)(nil)
	_ Store  = (*S3)(nil)
	_ Direct = (*S3)(nil)
)
