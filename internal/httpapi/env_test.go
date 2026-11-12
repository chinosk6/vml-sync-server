package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"vml-sync-server/internal/app"
	"vml-sync-server/internal/httpapi"
	"vml-sync-server/internal/meta"
	"vml-sync-server/internal/s3test"
	"vml-sync-server/internal/settings"
)

const (
	testAuthKey       = "test-client-key-0123456789abcdef"
	testAdminPassword = "test-admin-password-四月"

	// unit 是测试里“1 MB”换算成的字节数：默认的 chunk_mb: 5 就是 1000 字节一片，
	// 默认的 max_file_mb: 200 就是 40000 字节。这样可以拿很小的文件测分片逻辑。
	unit = 200
)

// clock 是可以手动拨动的时钟。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// options 决定测试环境怎么搭。零值 = 本地存储 + 中转 + 全部默认设置。
type options struct {
	s3             bool
	transfer       string
	maxFile        int64 // 字节，必须是 unit 的整数倍
	maxTotal       int64 // 字节，必须是 unit 的整数倍
	maxActive      int
	basePath       string
	noVerifyDirect bool
	trustForwarded bool
	accessLog      bool // 打访问日志（默认不打，免得刷屏）

	// 后台任务的间隔；留空用正式的默认值。
	cleanupInterval time.Duration
	sweepInterval   time.Duration
	retryInterval   time.Duration

	// fresh 为 true 时保持首次启动的原样：不改设置，管理员密码也还是生成的初始密码。
	fresh bool
	// dataDir 不为空时在这个已有的数据目录上启动（模拟重启），不再改任何设置。
	dataDir string
	// fake 不为空时沿用这个假 S3（模拟重启时用）。
	fake *s3test.Fake
	// clock 不为空时沿用这个时钟。
	clock *clock
}

// env 是一套跑起来的服务：真实的 HTTP 服务、真实的元数据库和存储（S3 用进程内的假服务）。
type env struct {
	t       *testing.T
	opts    options
	dataDir string
	db      *meta.DB
	app     *app.App
	fake    *s3test.Fake
	srv     *httptest.Server
	base    string
	clock   *clock

	adminCookie  *http.Cookie
	adminLoginAt time.Time
}

func newEnv(t *testing.T, o options) *env {
	t.Helper()
	e := &env{t: t, opts: o, clock: o.clock, fake: o.fake, dataDir: o.dataDir}
	if e.clock == nil {
		e.clock = &clock{t: time.Now().Truncate(time.Second)}
	}
	restart := e.dataDir != ""
	if !restart {
		e.dataDir = t.TempDir()
	}

	db, err := meta.Open(filepath.Join(e.dataDir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.db = db

	e.app, err = app.New(db, app.Options{
		DataDir:              e.dataDir,
		Now:                  e.clock.Now,
		BytesPerMB:           unit,
		PasswordIterations:   1000, // 正式的迭代次数每次登录要零点几秒，测试里用不着
		CleanupInterval:      o.cleanupInterval,
		SweepInterval:        o.sweepInterval,
		StorageRetryInterval: o.retryInterval,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.app.Close)

	if !restart && !o.fresh {
		// 用测试里固定的管理员密码和客户端密钥，再按 options 改设置。
		if err := e.app.ChangeAdminPassword(testAdminPassword); err != nil {
			t.Fatal(err)
		}
		patch := settings.Patch{AuthKey: ptr(testAuthKey)}
		if o.basePath != "" {
			patch.BasePath = ptr(o.basePath)
		}
		if o.trustForwarded {
			patch.TrustForwardedHeaders = ptr(true)
		}
		if o.noVerifyDirect {
			patch.VerifyDirectUploads = ptr(false)
		}
		if o.maxActive > 0 {
			patch.MaxActiveUploads = ptr(o.maxActive)
		}
		if o.maxFile > 0 {
			patch.MaxFileMB = ptr(o.maxFile / unit)
		}
		if o.maxTotal > 0 {
			patch.MaxTotalMB = ptr(o.maxTotal / unit)
		}
		if o.transfer != "" {
			patch.Transfer = ptr(o.transfer)
		}
		if o.s3 {
			if e.fake == nil {
				e.fake = s3test.New(t)
			}
			patch.Storage = e.s3Patch(s3test.AccessKey, s3test.SecretKey)
			patch.ConfirmStorageChange = true
		}
		if _, err := e.app.UpdateSettings(context.Background(), patch); err != nil {
			t.Fatalf("settings: %v", err)
		}
	}
	// 启动时的那一次对账（存储用不了时这里会失败，由用到它的测试自己检查）。
	e.app.Activate(context.Background())

	adminFS := fstest.MapFS{
		"index.html":  {Data: []byte("<!doctype html><title>admin</title>")},
		"app.js":      {Data: []byte("console.log('admin')")},
		"css/app.css": {Data: []byte("body{}")},
		".hidden":     {Data: []byte("secret")},
	}
	e.srv = httptest.NewServer(httpapi.New(e.app, adminFS, httpapi.Options{Now: e.clock.Now, Quiet: !o.accessLog}))
	t.Cleanup(e.srv.Close)
	e.base = e.srv.URL + e.app.Settings().BasePath
	return e
}

// s3Patch 返回“把存储设成假 S3”的设置更新。
func (e *env) s3Patch(accessKey, secretKey string) *settings.StoragePatch {
	return &settings.StoragePatch{
		Type: ptr(settings.StorageS3),
		S3: &settings.S3Patch{
			Endpoint:        ptr(e.fake.URL),
			Region:          ptr(s3test.Region),
			Bucket:          ptr(s3test.Bucket),
			AccessKeyID:     ptr(accessKey),
			SecretAccessKey: ptr(secretKey),
		},
	}
}

// restart 模拟重启：关掉服务，在同一个数据目录（和同一个假 S3、同一个时钟）上重新启动。
func (e *env) restart() *env {
	e.t.Helper()
	e.srv.Close()
	e.app.Close()
	e.db.Close()
	o := e.opts
	o.dataDir, o.fake, o.clock = e.dataDir, e.fake, e.clock
	return newEnv(e.t, o)
}

func ptr[T any](v T) *T { return &v }

// chunk 返回当前设置下一片有多少字节。
func (e *env) chunk() int { return int(e.app.Settings().ChunkMB) * unit }

// objectsDir 返回本地存储放模型文件的目录。
func (e *env) objectsDir() string { return filepath.Join(e.dataDir, "objects") }

// response 是一次请求的结果。
type response struct {
	t      *testing.T
	status int
	header http.Header
	body   []byte
}

// fields 把响应体当 JSON 对象解析。
func (r response) fields() map[string]any {
	r.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		r.t.Fatalf("response is not a JSON object: %v\n%s", err, r.body)
	}
	return m
}

func (r response) str(key string) string {
	r.t.Helper()
	v, _ := r.fields()[key].(string)
	return v
}

func (r response) num(key string) int64 {
	r.t.Helper()
	v, ok := r.fields()[key].(float64)
	if !ok {
		r.t.Fatalf("field %q is missing or not a number in %s", key, r.body)
	}
	return int64(v)
}

func (r response) has(key string) bool {
	r.t.Helper()
	_, ok := r.fields()[key]
	return ok
}

// want 检查状态码；不对就让测试失败。
func (r response) want(status int) response {
	r.t.Helper()
	if r.status != status {
		r.t.Fatalf("status = %d, want %d; body: %s", r.status, status, r.body)
	}
	return r
}

// wantState 检查 200 和 state 字段。
func (r response) wantState(state string) response {
	r.t.Helper()
	r.want(http.StatusOK)
	if got := r.str("state"); got != state {
		r.t.Fatalf("state = %q, want %q; body: %s", got, state, r.body)
	}
	return r
}

// wantError 检查状态码和 error 字段。
func (r response) wantError(status int, code string) response {
	r.t.Helper()
	r.want(status)
	if got := r.str("error"); got != code {
		r.t.Fatalf("error = %q, want %q; body: %s", got, code, r.body)
	}
	return r
}

// do 发一个请求。body 可以是 nil、[]byte（原样发）或任何能编码成 JSON 的值。
// url 以 "/" 开头时接在 {base} 后面，否则当作完整地址。
func (e *env) do(method, url string, body any, headers map[string]string) response {
	e.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	if len(url) > 0 && url[0] == '/' {
		url = e.base + url
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 不跟随重定向，测试要看到重定向本身。
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("%s %s: reading body: %v", method, url, err)
	}
	return response{t: e.t, status: resp.StatusCode, header: resp.Header, body: raw}
}

// call 以客户端的身份（带测试用的 auth_key）调接口。
func (e *env) call(method, path string, body any) response {
	e.t.Helper()
	return e.do(method, path, body, map[string]string{"Authorization": "Bearer " + testAuthKey})
}

// loginWith 用给定的密码登录，返回会话 Cookie。
func (e *env) loginWith(password string) *http.Cookie {
	e.t.Helper()
	r := e.do("POST", "/api/v1/admin/login", map[string]any{"password": password}, nil).want(200)
	resp := http.Response{Header: r.header}
	for _, c := range resp.Cookies() {
		if c.Name == "vml_admin" && c.Value != "" {
			return c
		}
	}
	e.t.Fatal("login did not set the vml_admin cookie")
	return nil
}

// login 以管理员身份登录，之后 admin() 会带上会话。
func (e *env) login() {
	e.t.Helper()
	e.adminCookie = e.loginWith(testAdminPassword)
	e.adminLoginAt = e.clock.Now()
}

// adminHeaders 返回管理请求要带的请求头（会话 Cookie 和 X-VML-Admin）。
func adminHeaders(c *http.Cookie) map[string]string {
	return map[string]string{"Cookie": c.Name + "=" + c.Value, "X-VML-Admin": "1"}
}

// admin 以管理员的身份调管理接口。
func (e *env) admin(method, path string, body any) response {
	e.t.Helper()
	// 测试把时钟拨过了会话有效期的话，重新登录。
	if e.adminCookie == nil || e.clock.Now().Sub(e.adminLoginAt) >= 12*time.Hour {
		e.login()
	}
	return e.do(method, "/api/v1/admin"+path, body, adminHeaders(e.adminCookie))
}

// putSettings 在管理页里改设置，要求成功。
func (e *env) putSettings(patch map[string]any) response {
	e.t.Helper()
	return e.admin("PUT", "/settings", patch).want(200)
}

// bundle 造一个 n 字节的“资源包”（以 UnityFS 开头的随机内容），返回内容和它的模型 id。
func bundle(t *testing.T, n int) ([]byte, string) {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	copy(data, "UnityFS")
	return data, idOf(data)
}

func idOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

// begin 开始上传。extra 里的键会并进请求体。
func (e *env) begin(id string, size int, extra map[string]any) response {
	e.t.Helper()
	body := map[string]any{"size": size, "name": "model.vrca", "uploader": "tester"}
	for k, v := range extra {
		body[k] = v
	}
	return e.call("POST", "/api/v1/models/"+id+"/upload", body)
}

// part 返回 content 的第 n 片（从 1 开始），按当前设置的分片大小切。
func (e *env) part(content []byte, n int) []byte { return slice(content, n, e.chunk()) }

func (e *env) partCount(content []byte) int { return (len(content) + e.chunk() - 1) / e.chunk() }

// slice 返回 content 按 chunk 字节一片切出来的第 n 片。
func slice(content []byte, n, chunk int) []byte {
	start := (n - 1) * chunk
	return content[start:min(start+chunk, len(content))]
}

// putPart 用中转方式上传第 n 片。
func (e *env) putPart(uploadID string, content []byte, n int) response {
	e.t.Helper()
	return e.call("PUT", "/api/v1/uploads/"+uploadID+"/parts/"+strconv.Itoa(n), e.part(content, n))
}

// putDirect 用直连方式上传一片：先要临时地址，再对它发不带任何额外请求头的 PUT。
func (e *env) putDirect(uploadID string, n int, data []byte) {
	e.t.Helper()
	r := e.call("POST", "/api/v1/uploads/"+uploadID+"/parts/"+strconv.Itoa(n)+"/url", nil).want(200)
	if r.num("expires_in") != int64(e.app.Settings().DirectURLTTLSeconds) {
		e.t.Fatalf("expires_in = %d", r.num("expires_in"))
	}
	put := e.do("PUT", r.str("url"), data, nil)
	if put.status/100 != 2 {
		e.t.Fatalf("direct PUT of part %d: status %d: %s", n, put.status, put.body)
	}
}

// upload 按会话的传输方式把 content 的各片传上去（skip 里的片号不传），返回 upload_id。
func (e *env) upload(id string, content []byte, extra map[string]any, skip ...int) string {
	e.t.Helper()
	r := e.begin(id, len(content), extra).wantState("upload")
	uploadID := r.str("upload_id")
	if int(r.num("part_count")) != e.partCount(content) || int(r.num("chunk_bytes")) != e.chunk() {
		e.t.Fatalf("unexpected upload plan: %s", r.body)
	}
	skipped := map[int]bool{}
	for _, n := range skip {
		skipped[n] = true
	}
	for n := 1; n <= e.partCount(content); n++ {
		if skipped[n] {
			continue
		}
		if r.str("mode") == "direct" {
			e.putDirect(uploadID, n, e.part(content, n))
		} else {
			e.putPart(uploadID, content, n).want(200)
		}
	}
	return uploadID
}

func (e *env) complete(uploadID string) response {
	e.t.Helper()
	return e.call("POST", "/api/v1/uploads/"+uploadID+"/complete", nil)
}

// mustUpload 完整地传一个模型并确认它变成 ready。
func (e *env) mustUpload(id string, content []byte, extra map[string]any) {
	e.t.Helper()
	uploadID := e.upload(id, content, extra)
	r := e.complete(uploadID).wantState("ready")
	if r.str("id") != id {
		e.t.Fatalf("complete returned id %q, want %q", r.str("id"), id)
	}
}

func (e *env) status(id string) response {
	e.t.Helper()
	return e.call("GET", "/api/v1/models/"+id, nil).want(200)
}

// wantDownloadable 确认一个模型能下载，而且内容对得上。
func (e *env) wantDownloadable(id string, content []byte) {
	e.t.Helper()
	if got := e.status(id).str("state"); got != "ready" {
		e.t.Fatalf("status state = %q, want ready", got)
	}
	if got := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200).body; !bytes.Equal(got, content) {
		e.t.Fatalf("model %s: downloaded content differs", id)
	}
}

// wantNotDownloadable 确认一个 id 的内容通过任何途径都拿不到。
func (e *env) wantNotDownloadable(id, state string) {
	e.t.Helper()
	if got := e.status(id).str("state"); got != state {
		e.t.Fatalf("status state = %q, want %q", got, state)
	}
	if got := e.call("POST", "/api/v1/models/"+id+"/download", nil).want(200).str("state"); got != state {
		e.t.Fatalf("download state = %q, want %q", got, state)
	}
	e.call("POST", "/api/v1/models/"+id+"/download", map[string]any{"mode": "proxy"}).wantState(state)
	e.call("GET", "/api/v1/models/"+id+"/file", nil).wantError(404, "not_found")
}

// tempFiles 返回上传临时目录里还剩几个文件。
func (e *env) tempFiles() int {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.dataDir, "uploads"))
	if err != nil {
		e.t.Fatal(err)
	}
	return len(entries)
}

// localObjects 返回本地存储目录里有几个文件。
func (e *env) localObjects() int {
	e.t.Helper()
	entries, err := os.ReadDir(e.objectsDir())
	if err != nil {
		e.t.Fatal(err)
	}
	return len(entries)
}

func describe(v any) string { return fmt.Sprintf("%#v", v) }
