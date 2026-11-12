package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vml-sync-server/internal/s3"
	"vml-sync-server/internal/s3test"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func idOf(data []byte) (string, []byte) {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16]), sum[:]
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload.part")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newS3Store(t *testing.T) (*S3, *s3test.Fake) {
	t.Helper()
	fake := s3test.New(t)
	client, err := s3.New(s3.Config{
		Endpoint:        fake.URL,
		Region:          s3test.Region,
		Bucket:          s3test.Bucket,
		AccessKeyID:     s3test.AccessKey,
		SecretAccessKey: s3test.SecretKey,
		PathStyle:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewS3(client, "models/"), fake
}

// 两种存储都要通过的基本行为。
func testStore(t *testing.T, store Store) {
	ctx := context.Background()
	data := randomBytes(t, 300_000)
	id, sum := idOf(data)

	// 探测：能写、能读回、能删，而且不留下任何东西（不会被当成模型列出来）。
	if err := store.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if list, err := store.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("List after Probe = %+v, %v", list, err)
	}

	if _, err := store.Stat(ctx, id); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat on missing object: %v", err)
	}
	if _, err := store.Open(ctx, id, 0, -1); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Open on missing object: %v", err)
	}
	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete on missing object: %v", err)
	}

	if err := store.PutFile(ctx, id, writeTemp(t, data), int64(len(data)), sum); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	obj, err := store.Stat(ctx, id)
	if err != nil || obj.Size != int64(len(data)) || obj.ID != id {
		t.Fatalf("Stat = %+v, %v", obj, err)
	}
	if time.Since(obj.ModTime) > time.Hour || obj.ModTime.IsZero() {
		t.Fatalf("unexpected mod time %v", obj.ModTime)
	}

	read := func(offset, length int64) []byte {
		t.Helper()
		rc, err := store.Open(ctx, id, offset, length)
		if err != nil {
			t.Fatalf("Open(%d, %d): %v", offset, length, err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return got
	}
	if got := read(0, -1); !bytes.Equal(got, data) {
		t.Fatal("full read differs")
	}
	if got := read(1000, -1); !bytes.Equal(got, data[1000:]) {
		t.Fatal("read from offset differs")
	}
	if got := read(1000, 500); !bytes.Equal(got, data[1000:1500]) {
		t.Fatal("ranged read differs")
	}
	if got := read(0, 7); !bytes.Equal(got, data[:7]) {
		t.Fatal("head read differs")
	}

	list, err := store.List(ctx)
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Size != int64(len(data)) {
		t.Fatalf("List = %+v, %v", list, err)
	}

	// 不合法的 id 不能参与拼路径 / 对象名。
	for _, bad := range []string{"", "../etc/passwd", "ABCDEF0123456789ABCDEF0123456789", id + "0", id[:31] + "/"} {
		if _, err := store.Open(ctx, bad, 0, -1); err == nil || errors.Is(err, ErrNotExist) {
			t.Fatalf("Open(%q) should be refused, got %v", bad, err)
		}
		if err := store.Delete(ctx, bad); err == nil {
			t.Fatalf("Delete(%q) should be refused", bad)
		}
	}

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Stat(ctx, id); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Stat after delete: %v", err)
	}
	if list, err := store.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("List after delete = %+v, %v", list, err)
	}
}

func TestLocalStore(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	testStore(t, store)

	// 名字不是合法 id 的文件不算对象；残留的临时文件会被清掉。
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, tmpPrefix+"deadbeef"), []byte("x"), 0o644)
	if list, err := store.List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := store.CleanupStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, tmpPrefix+"deadbeef")); !os.IsNotExist(err) {
		t.Fatal("stale temporary file was not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal("unrelated file must not be touched")
	}
}

func TestS3Store(t *testing.T) {
	store, fake := newS3Store(t)
	// 桶里别人的东西、前缀之外的东西都不算对象。
	fake.PutObject(t, "other/0123456789abcdef0123456789abcdef", []byte("x"))
	fake.PutObject(t, "models/readme.txt", []byte("x"))
	testStore(t, store)
}

// 探测不能用的存储：报错，而且错误里分得清是凭据不对还是桶不存在；不留下探测对象。
func TestS3ProbeFailures(t *testing.T) {
	_, fake := newS3Store(t)
	open := func(bucket, secret string) *S3 {
		client, err := s3.New(s3.Config{
			Endpoint: fake.URL, Region: s3test.Region, Bucket: bucket,
			AccessKeyID: s3test.AccessKey, SecretAccessKey: secret, PathStyle: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return NewS3(client, "models/")
	}
	ctx := context.Background()

	var se *s3.Error
	if err := open(s3test.Bucket, "wrong-secret").Probe(ctx); !errors.As(err, &se) || se.Code != "SignatureDoesNotMatch" {
		t.Fatalf("probe with a wrong secret: %v", err)
	}
	if err := open("no-such-bucket", s3test.SecretKey).Probe(ctx); !errors.As(err, &se) || se.Code != "NoSuchBucket" {
		t.Fatalf("probe of a missing bucket: %v", err)
	}
	// 写进去了但读不回来：也算不能用，探测对象照样被删掉。
	fake.FailNext("GET", "_probe-", 1)
	if err := open(s3test.Bucket, s3test.SecretKey).Probe(ctx); err == nil {
		t.Fatal("probe succeeded although the object could not be read back")
	}
	if err := open(s3test.Bucket, s3test.SecretKey).Probe(ctx); err != nil {
		t.Fatalf("probe of a working store: %v", err)
	}
	if keys := fake.Keys(t); len(keys) != 0 {
		t.Fatalf("probe objects left behind: %v", keys)
	}
}

// 直连上传的完整过程：分片上传到暂存位置 → 拼起来 → 读回来 → 复制到最终位置。
func TestS3Direct(t *testing.T) {
	store, fake := newS3Store(t)
	ctx := context.Background()

	const chunk = 4096
	data := randomBytes(t, chunk*2+123)
	id, _ := idOf(data)
	uploadID := "0123456789abcdef0123456789abcdef"

	handle, err := store.Begin(ctx, uploadID)
	if err != nil {
		t.Fatal(err)
	}
	put := func(part int, body []byte) {
		t.Helper()
		u, err := store.PartURL(uploadID, handle, part, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		// 和真实的客户端一样：只发 PUT 和内容，不带任何额外的请求头。
		req, _ := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("PUT part %d: status %d", part, resp.StatusCode)
		}
	}
	put(1, data[:chunk])
	put(3, data[2*chunk:])

	parts, err := store.Parts(ctx, uploadID, handle)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Number != 1 || parts[0].Size != chunk || parts[1].Number != 3 || parts[1].Size != 123 {
		t.Fatalf("Parts = %+v", parts)
	}

	put(2, data[chunk:2*chunk])
	parts, err = store.Parts(ctx, uploadID, handle)
	if err != nil || len(parts) != 3 {
		t.Fatalf("Parts = %+v, %v", parts, err)
	}
	if err := store.Assemble(ctx, uploadID, handle, parts); err != nil {
		t.Fatal(err)
	}

	// 拼好之后内容在暂存位置，最终位置上还什么都没有。
	if _, ok := fake.Object("models/" + id); ok {
		t.Fatal("object appeared at the final key before it was promoted")
	}
	size, err := store.StatStaged(ctx, uploadID)
	if err != nil || size != int64(len(data)) {
		t.Fatalf("StatStaged = %d, %v", size, err)
	}
	rc, err := store.OpenStaged(ctx, uploadID, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("staged content differs")
	}

	if err := store.Promote(ctx, uploadID, id); err != nil {
		t.Fatal(err)
	}
	if final, ok := fake.Object("models/" + id); !ok || !bytes.Equal(final, data) {
		t.Fatal("final object missing or different after Promote")
	}
	if _, ok := fake.Object("models/" + stagingDir + uploadID); ok {
		t.Fatal("staged object was not removed after Promote")
	}

	// 下载用的临时地址：直接 GET，不带任何请求头。
	u, err := store.DownloadURL(id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatalf("presigned GET: status %d, %d bytes", resp.StatusCode, len(body))
	}

	// 被改过的临时地址不能用。
	resp, err = http.Get(u[:len(u)-4] + "0000")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered presigned GET: status %d", resp.StatusCode)
	}
}

// 启动时的清理：没完成的分片上传被中止，暂存对象被删掉，正式对象不受影响。
func TestS3CleanupStale(t *testing.T) {
	store, fake := newS3Store(t)
	ctx := context.Background()

	if err := store.CleanupStale(ctx); err != nil {
		t.Fatalf("CleanupStale on an empty bucket: %v", err)
	}

	fake.PutObject(t, "models/0123456789abcdef0123456789abcdef", []byte("keep"))
	fake.PutObject(t, "models/"+stagingDir+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []byte("stale"))
	uploadID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	handle, err := store.Begin(ctx, uploadID)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.CleanupStale(ctx); err != nil {
		t.Fatal(err)
	}
	keys := fake.Keys(t)
	if len(keys) != 1 || keys[0] != "models/0123456789abcdef0123456789abcdef" {
		t.Fatalf("keys after cleanup = %v", keys)
	}
	if _, err := store.Parts(ctx, uploadID, handle); !errors.Is(err, ErrNotExist) {
		t.Fatalf("multipart upload should have been aborted, Parts returned %v", err)
	}
	// 再中止一次不算错。
	if err := store.Abort(ctx, uploadID, handle); err != nil {
		t.Fatalf("Abort on a missing upload: %v", err)
	}
}
