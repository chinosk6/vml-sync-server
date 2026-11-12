package httpapi_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"vml-sync-server/internal/s3test"
)

// eventually 反复检查一个条件，直到成立或者超时。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 一片传到一半连接断了：这一片不算数（哪怕之前完整传过），补传之后才能完成。
func TestInterruptedPartDoesNotCount(t *testing.T) {
	e := newEnv(t, options{})
	content, id := bundle(t, 2500)
	uploadID := e.upload(id, content, nil)
	if got := e.status(id).num("received"); got != 2500 {
		t.Fatalf("received = %d", got)
	}

	// 重传第 2 片，声明 1000 字节，只发 400 字节垃圾就断开。
	conn, err := net.Dial("tcp", e.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "PUT /api/v1/uploads/%s/parts/2 HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\n\r\n",
		uploadID, testAuthKey)
	conn.Write(bytes.Repeat([]byte{0xee}, 400))
	eventually(t, "the interrupted part to stop counting", func() bool {
		return e.status(id).num("received") == 1500
	})
	conn.Close()

	// 临时文件里现在有 400 字节垃圾，但服务器知道这一片不完整。
	eventually(t, "complete to report the part as missing", func() bool {
		r := e.complete(uploadID).want(200)
		return r.str("state") == "incomplete" && fmt.Sprint(r.fields()["missing"]) == "[2]"
	})
	e.putPart(uploadID, content, 2).want(200)
	e.complete(uploadID).wantState("ready")
	if got := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200).body; !bytes.Equal(got, content) {
		t.Fatal("content differs")
	}
}

// 不看请求体的接口，客户端照样带了请求体、而且每个请求都用新连接（Connection: close）：
// 响应必须完整送到，连接不能被重置。
func TestUnreadRequestBodyDoesNotResetConnection(t *testing.T) {
	e := newEnv(t, options{})
	e.login()
	addr := e.srv.Listener.Addr().String()

	// send 发一个原始请求（先发请求头、再单独发请求体，和很多 HTTP 库的做法一样），读回完整的响应。
	send := func(method, path, headers, body string) string {
		t.Helper()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: test\r\nConnection: close\r\nContent-Length: %d\r\n%s\r\n",
			method, path, len(body), headers)
		io.WriteString(conn, body)
		raw, err := io.ReadAll(conn)
		if err != nil {
			t.Fatalf("%s %s: reading the response: %v (got %q)", method, path, err, raw)
		}
		return string(raw)
	}

	auth := "Authorization: Bearer " + testAuthKey + "\r\n"
	admin := "Cookie: vml_admin=" + e.adminCookie.Value + "\r\nX-VML-Admin: 1\r\n"
	big := strings.Repeat("x", 100_000)
	for i := 0; i < 150; i++ {
		for _, tc := range []struct {
			method, path, headers, body, want string
		}{
			{"POST", "/api/v1/uploads/ffffffffffffffffffffffffffffffff/abort", auth, "{}", `{"ok":true}`},
			{"POST", "/api/v1/uploads/ffffffffffffffffffffffffffffffff/complete", auth, "{}", `"upload_gone"`},
			{"POST", "/api/v1/uploads/ffffffffffffffffffffffffffffffff/parts/1/url", auth, "{}", `"upload_gone"`},
			{"POST", "/api/v1/admin/cleanup", admin, "{}", `"freed_bytes":0`},
			{"GET", "/api/v1/ping", auth, "{}", `"ok":true`},
			{"POST", "/api/v1/ping", "", big, `"unauthorized"`},
			{"POST", "/api/v1/nope", auth, big, `"not_found"`},
		} {
			if got := send(tc.method, tc.path, tc.headers, tc.body); !strings.Contains(got, tc.want) {
				t.Fatalf("round %d: %s %s: response %q does not contain %q", i, tc.method, tc.path, got, tc.want)
			}
		}
	}
}

// 同一个会话同时来了好几个 complete：都得到同样的结论，模型只登记一次。
func TestConcurrentCompletes(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts options
	}{
		{"proxy local", options{}},
		{"direct s3", options{s3: true, transfer: "direct"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts)
			content, id := bundle(t, 3500)
			uploadID := e.upload(id, content, nil)

			const n = 8
			states := make([]string, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					states[i] = e.complete(uploadID).want(200).str("state")
				}()
			}
			wg.Wait()
			for _, s := range states {
				if s != "ready" {
					t.Fatalf("states = %v", states)
				}
			}
			if total := e.admin("GET", "/models", nil).num("total"); total != 1 {
				t.Fatalf("total = %d", total)
			}
			if got := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200).body; !bytes.Equal(got, content) {
				t.Fatal("content differs")
			}
			if e.fake != nil {
				if keys := e.fake.Keys(t); len(keys) != 1 {
					t.Fatalf("keys = %v", keys)
				}
			}
		})
	}
}

// 许多模型同时上传、下载、被删除：不出错、不串内容、不留垃圾。
func TestParallelTraffic(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts options
	}{
		{"proxy local", options{maxActive: 64}},
		{"direct s3", options{s3: true, transfer: "direct", maxActive: 64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts)
			e.login()
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					content, id := bundle(t, 1500+i*333)
					for round := 0; round < 3; round++ {
						r := e.begin(id, len(content), nil).wantState("upload")
						uploadID := r.str("upload_id")
						for n := 1; n <= e.partCount(content); n++ {
							if r.str("mode") == "direct" {
								e.putDirect(uploadID, n, e.part(content, n))
							} else {
								e.putPart(uploadID, content, n).want(200)
							}
						}
						e.complete(uploadID).wantState("ready")
						e.call("POST", "/api/v1/models/"+id+"/download", nil).wantState("ready")
						if got := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200).body; !bytes.Equal(got, content) {
							t.Errorf("model %s: content differs", id)
						}
						e.admin("POST", "/models/delete", map[string]any{"ids": []string{id}}).want(200)
						if got := e.status(id).str("state"); got != "missing" {
							t.Errorf("model %s: state after delete = %q", id, got)
						}
					}
				}()
			}
			wg.Wait()
			if total := e.admin("GET", "/models", nil).num("total"); total != 0 {
				t.Fatalf("total = %d", total)
			}
			if n := e.tempFiles(); n != 0 {
				t.Fatalf("%d temporary file(s) left behind", n)
			}
			if e.fake != nil {
				if keys := e.fake.Keys(t); len(keys) != 0 {
					t.Fatalf("keys = %v", keys)
				}
			}
		})
	}
}

// 后台任务：启动时清理一次，之后定时做过期清理和上传会话清理。
func TestBackgroundLoop(t *testing.T) {
	e := newEnv(t, options{cleanupInterval: 30 * time.Millisecond, sweepInterval: 10 * time.Millisecond})

	old, oldID := bundle(t, 1200)
	e.mustUpload(oldID, old, nil)
	e.clock.Advance(73 * time.Hour)
	stuck, stuckID := bundle(t, 2500)
	e.upload(stuckID, stuck, nil, 3)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.app.Run(ctx)
	}()

	// 启动时那一轮清理删掉了过期的模型。
	eventually(t, "the expired model to be removed", func() bool {
		return e.status(oldID).str("state") == "missing"
	})
	// 清理时间是在一轮清理结束时记下的，比“模型不见了”稍晚一点。
	eventually(t, "the cleanup time to be recorded", func() bool {
		ov := e.admin("GET", "/overview", nil).want(200)
		return ov.num("last_cleanup_at") == e.clock.Now().Unix() && ov.num("next_cleanup_at") > 0
	})
	if got := e.status(stuckID).str("state"); got != "uploading" {
		t.Fatalf("state of the active upload = %q", got)
	}

	// 上传会话空闲超时后被后台任务清掉。
	e.clock.Advance(121 * time.Second)
	eventually(t, "the idle upload session to be removed", func() bool {
		return e.status(stuckID).str("state") == "missing"
	})

	// 之后过期的模型由定时的那一轮清理删掉。
	e.mustUpload(oldID, old, nil)
	e.clock.Advance(73 * time.Hour)
	eventually(t, "the periodic cleanup to remove the model", func() bool {
		return e.status(oldID).str("state") == "missing"
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("background loop did not stop")
	}
}

// 日志里不能出现密钥、密码、会话令牌、临时地址的签名。
func TestLogsCarryNoSecrets(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	defer log.SetOutput(os.Stderr)

	e := newEnv(t, options{s3: true, transfer: "direct", accessLog: true, basePath: "/hidden"})
	content, id := bundle(t, 2500)
	e.mustUpload(id, content, nil)
	dl := e.call("POST", "/api/v1/models/"+id+"/download", nil).wantState("ready")
	e.do("GET", dl.str("url"), nil, nil).want(200)
	e.do("GET", "/api/v1/ping", nil, map[string]string{"Authorization": "Bearer wrong-" + testAuthKey}).want(401)
	e.do("POST", "/api/v1/admin/login", map[string]any{"password": "wrong-" + testAdminPassword}, nil).want(401)
	e.admin("GET", "/models?q="+id, nil).want(200)
	// 一次失败的上传（会进错误日志）。
	bad, badID := bundle(t, 1500)
	uploadID := e.upload(badID, bad, nil)
	e.fake.FailNext("GET", "uploadId=", 1)
	e.complete(uploadID).wantError(500, "internal")
	e.complete(uploadID).wantState("ready")

	mu.Lock()
	logged := buf.String()
	mu.Unlock()
	if !strings.Contains(logged, `"/hidden/api/v1/models/`+id+`/download" 200`) || !strings.Contains(logged, "error:") {
		t.Fatalf("expected access and error log lines, got:\n%s", logged)
	}
	for _, secret := range []string{
		testAuthKey, testAdminPassword, e.adminCookie.Value, s3test.SecretKey,
		"X-Amz-Signature", "X-Amz-Credential", "Bearer",
	} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log contains %q:\n%s", secret, logged)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
