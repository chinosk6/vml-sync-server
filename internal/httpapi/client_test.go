package httpapi_test

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vml-sync-server/internal/s3test"
	"vml-sync-server/internal/version"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// 中转 + 本地存储：上传 → 查询 → 下载的完整流程，逐字段对照 docs/API.md。
func TestProxyLocalFullFlow(t *testing.T) {
	e := newEnv(t, options{})
	content, id := bundle(t, 2500) // 1000 + 1000 + 500 三片

	ping := e.call("GET", "/api/v1/ping", nil).want(200)
	if ping.fields()["ok"] != true || ping.str("name") != "vml-sync-server" || ping.str("version") != version.Version ||
		ping.num("api") != 1 || ping.str("transfer") != "proxy" ||
		ping.num("max_file_bytes") != 200*unit || ping.num("chunk_bytes") != 1000 {
		t.Fatalf("ping = %s", ping.body)
	}
	if ping.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", ping.header.Get("Cache-Control"))
	}

	st := e.status(id)
	if st.str("state") != "missing" || st.str("id") != id || len(st.fields()) != 2 {
		t.Fatalf("status before upload = %s", st.body)
	}

	begin := e.begin(id, len(content), map[string]any{"name": "Chiffon.vrca", "uploader": "小明"}).wantState("upload")
	uploadID := begin.str("upload_id")
	if !hex32.MatchString(uploadID) || begin.str("mode") != "proxy" || begin.num("chunk_bytes") != 1000 || begin.num("part_count") != 3 {
		t.Fatalf("begin = %s", begin.body)
	}

	if got := e.putPart(uploadID, content, 1).want(200).num("received"); got != 1000 {
		t.Fatalf("received after part 1 = %d", got)
	}
	st = e.status(id)
	if st.str("state") != "uploading" || st.num("size") != 2500 || st.num("received") != 1000 {
		t.Fatalf("status while uploading = %s", st.body)
	}
	// 不要求按顺序。
	if got := e.putPart(uploadID, content, 3).want(200).num("received"); got != 1500 {
		t.Fatalf("received after part 3 = %d", got)
	}
	if got := e.putPart(uploadID, content, 2).want(200).num("received"); got != 2500 {
		t.Fatalf("received after part 2 = %d", got)
	}
	// 重传同一片是幂等的。
	if got := e.putPart(uploadID, content, 2).want(200).num("received"); got != 2500 {
		t.Fatalf("received after re-sending part 2 = %d", got)
	}

	done := e.complete(uploadID).wantState("ready")
	if done.str("id") != id {
		t.Fatalf("complete = %s", done.body)
	}
	// 响应丢了、客户端又调了一次 complete：拿到同样的结果。
	e.complete(uploadID).wantState("ready")

	st = e.status(id)
	if st.str("state") != "ready" || st.num("size") != 2500 || st.has("received") {
		t.Fatalf("status after upload = %s", st.body)
	}

	dl := e.call("POST", "/api/v1/models/"+id+"/download", nil).wantState("ready")
	if dl.str("mode") != "proxy" || dl.num("size") != 2500 || dl.has("url") {
		t.Fatalf("download = %s", dl.body)
	}
	file := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200)
	if !bytes.Equal(file.body, content) {
		t.Fatal("downloaded content differs from what was uploaded")
	}
	if file.header.Get("Content-Type") != "application/octet-stream" || file.header.Get("Content-Length") != "2500" ||
		file.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("file headers = %v", file.header)
	}

	// 上传完成后会话没了，临时文件也没了。
	e.putPart(uploadID, content, 1).wantError(404, "upload_gone")
	if n := e.tempFiles(); n != 0 {
		t.Fatalf("%d temporary file(s) left behind", n)
	}

	// 管理页看到的记录。
	list := e.admin("GET", "/models", nil).want(200)
	items := list.fields()["items"].([]any)
	if list.num("total") != 1 || len(items) != 1 {
		t.Fatalf("admin list = %s", list.body)
	}
	item := items[0].(map[string]any)
	now := float64(e.clock.Now().Unix())
	if item["id"] != id || item["name"] != "Chiffon.vrca" || item["uploader"] != "小明" || item["size"] != float64(2500) ||
		item["uploaded_at"] != now || item["last_download_at"] != now || item["download_count"] != float64(1) ||
		item["expires_at"] != now+72*3600 {
		t.Fatalf("admin item = %s", describe(item))
	}
}

// 同一个文件再传一次：云端已经有了，直接 ready。文件接口不计下载次数。
func TestReuploadIsReady(t *testing.T) {
	e := newEnv(t, options{})
	content, id := bundle(t, 1500)
	e.mustUpload(id, content, nil)

	again := e.begin(id, len(content), nil).wantState("ready")
	if again.has("upload_id") {
		t.Fatalf("begin on an existing model = %s", again.body)
	}
	// 哪怕它现在超过了上限，已经有的也还是 ready。
	e.putSettings(map[string]any{"max_file_mb": 1})
	e.begin(id, len(content), nil).wantState("ready")

	for i := 0; i < 3; i++ {
		e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200)
		e.status(id)
	}
	item := e.admin("GET", "/models", nil).fields()["items"].([]any)[0].(map[string]any)
	if item["download_count"] != float64(0) || item["last_download_at"] != float64(0) {
		t.Fatalf("status / file requests must not count as downloads: %s", describe(item))
	}
}

// 别人正在传同一个 id、或者会话总数到了上限：busy。
func TestConcurrentUploadIsBusy(t *testing.T) {
	e := newEnv(t, options{maxActive: 2})
	content, id := bundle(t, 2500)

	first := e.begin(id, len(content), nil).wantState("upload")
	e.putPart(first.str("upload_id"), content, 1).want(200)

	busy := e.begin(id, len(content), nil).wantState("busy")
	if busy.num("retry_after") <= 0 || busy.num("size") != 2500 || busy.num("received") != 1000 || busy.has("upload_id") {
		t.Fatalf("busy = %s", busy.body)
	}

	// 第二个会话占满上限，第三个模型进不来。
	other, otherID := bundle(t, 800)
	e.begin(otherID, len(other), nil).wantState("upload")
	third, thirdID := bundle(t, 800)
	full := e.begin(thirdID, len(third), nil).wantState("busy")
	if full.num("retry_after") <= 0 {
		t.Fatalf("busy (limit) = %s", full.body)
	}

	// 第一个传完之后腾出了位置。
	e.putPart(first.str("upload_id"), content, 2).want(200)
	e.putPart(first.str("upload_id"), content, 3).want(200)
	e.complete(first.str("upload_id")).wantState("ready")
	e.begin(thirdID, len(third), nil).wantState("upload")
}

// 许多请求同时为同一个 id 开始上传：只有一个拿到会话。
func TestSimultaneousBeginsGetOneSession(t *testing.T) {
	e := newEnv(t, options{maxActive: 64})
	content, id := bundle(t, 3000)

	const n = 16
	states := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			states[i] = e.begin(id, len(content), nil).want(200).str("state")
		}()
	}
	wg.Wait()
	uploads := 0
	for _, s := range states {
		switch s {
		case "upload":
			uploads++
		case "busy":
		default:
			t.Fatalf("unexpected state %q", s)
		}
	}
	if uploads != 1 {
		t.Fatalf("%d requests got an upload session, want exactly 1 (%v)", uploads, states)
	}
}

// 缺片：incomplete，会话保留；补传之后成功。
func TestIncompleteThenResume(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts options
	}{
		{"proxy local", options{}},
		{"proxy s3", options{s3: true}},
		{"direct s3", options{s3: true, transfer: "direct"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts)
			content, id := bundle(t, 3500) // 4 片
			uploadID := e.upload(id, content, nil, 2, 4)

			r := e.complete(uploadID).wantState("incomplete")
			missing := r.fields()["missing"].([]any)
			if len(missing) != 2 || missing[0] != float64(2) || missing[1] != float64(4) {
				t.Fatalf("missing = %s", r.body)
			}
			if got := e.status(id).str("state"); got != "uploading" {
				t.Fatalf("state after incomplete = %q", got)
			}

			if tc.opts.transfer == "direct" {
				e.putDirect(uploadID, 2, e.part(content, 2))
				e.putDirect(uploadID, 4, e.part(content, 4))
			} else {
				e.putPart(uploadID, content, 2).want(200)
				e.putPart(uploadID, content, 4).want(200)
			}
			e.complete(uploadID).wantState("ready")
			if got := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200).body; !bytes.Equal(got, content) {
				t.Fatal("content differs")
			}
		})
	}
}

// 没通过核对的内容：rejected，而且通过任何途径都下载不到。
func TestRejectedUploadsAreNeverDownloadable(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts options
	}{
		{"proxy local", options{}},
		{"proxy s3", options{s3: true}},
		{"direct s3", options{s3: true, transfer: "direct"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts)

			// 内容是资源包，但哈希和声称的 id 对不上。
			real, realID := bundle(t, 2500)
			fake := bytes.Clone(real)
			fake[len(fake)-1] ^= 0xff
			uploadID := e.upload(realID, fake, nil)
			r := e.complete(uploadID).wantState("rejected")
			if r.str("reason") != "hash_mismatch" {
				t.Fatalf("rejected = %s", r.body)
			}
			e.wantNotDownloadable(realID, "missing")
			// 拒收之后会话作废；重复调用得到同样的结论。
			e.complete(uploadID).wantState("rejected")

			// id 和内容对得上，但不是资源包。
			junk, _ := bundle(t, 1800)
			copy(junk, "NotUnity")
			junkID := idOf(junk)
			r = e.complete(e.upload(junkID, junk, nil)).wantState("rejected")
			if r.str("reason") != "not_a_bundle" {
				t.Fatalf("rejected = %s", r.body)
			}
			e.wantNotDownloadable(junkID, "missing")

			// 被拒收的东西不留痕迹。
			if n := e.tempFiles(); n != 0 {
				t.Fatalf("%d temporary file(s) left behind", n)
			}
			if e.fake != nil {
				if keys := e.fake.Keys(t); len(keys) != 0 {
					t.Fatalf("objects left in the bucket: %v", keys)
				}
			}
			if list := e.admin("GET", "/models", nil); list.num("total") != 0 {
				t.Fatalf("admin list = %s", list.body)
			}

			// 同一个 id 之后还能正常上传。
			e.mustUpload(realID, real, nil)
			if got := e.call("GET", "/api/v1/models/"+realID+"/file", nil).want(200).body; !bytes.Equal(got, real) {
				t.Fatal("content differs")
			}
		})
	}
}

// 直连模式下某一片的长度不对：size_mismatch。
func TestDirectWrongPartSizeIsRejected(t *testing.T) {
	e := newEnv(t, options{s3: true, transfer: "direct"})
	content, id := bundle(t, 2500)
	uploadID := e.begin(id, len(content), nil).wantState("upload").str("upload_id")
	e.putDirect(uploadID, 1, e.part(content, 1))
	e.putDirect(uploadID, 2, e.part(content, 2))
	e.putDirect(uploadID, 3, e.part(content, 3)[:400]) // 应该是 500 字节

	r := e.complete(uploadID).wantState("rejected")
	if r.str("reason") != "size_mismatch" {
		t.Fatalf("rejected = %s", r.body)
	}
	e.wantNotDownloadable(id, "missing")
	if keys := e.fake.Keys(t); len(keys) != 0 {
		t.Fatalf("objects left in the bucket: %v", keys)
	}
}

// 超过单个文件上限、超过总容量上限。
func TestTooLargeAndStorageFull(t *testing.T) {
	e := newEnv(t, options{maxFile: 5000, maxTotal: 6000})
	_, id := bundle(t, 100)

	r := e.begin(id, 5001, nil).wantState("too_large")
	if r.num("max_file_bytes") != 5000 {
		t.Fatalf("too_large = %s", r.body)
	}
	if got := e.status(id).str("state"); got != "missing" {
		t.Fatalf("state = %q", got)
	}

	a, aID := bundle(t, 4000)
	e.mustUpload(aID, a, nil)
	b, bID := bundle(t, 2001)
	e.begin(bID, len(b), nil).wantState("storage_full")
	// 正在传的也算进总量。
	c, cID := bundle(t, 1500)
	e.begin(cID, len(c), nil).wantState("upload")
	d, dID := bundle(t, 600)
	e.begin(dID, len(d), nil).wantState("storage_full")
	_, eID := bundle(t, 500)
	e.begin(eID, 500, nil).wantState("upload")
}

// S3 存储下的中转：完整流程，文件接口从对象存储读出来转发。
func TestS3ProxyFullFlow(t *testing.T) {
	e := newEnv(t, options{s3: true})
	content, id := bundle(t, 4321)

	if got := e.call("GET", "/api/v1/ping", nil).want(200).str("transfer"); got != "proxy" {
		t.Fatalf("transfer = %q", got)
	}
	// 服务器默认中转时忽略请求里的 mode。
	uploadID := e.upload(id, content, map[string]any{"mode": "direct"})
	e.call("POST", "/api/v1/uploads/"+uploadID+"/parts/1/url", nil).wantError(409, "wrong_mode")
	e.complete(uploadID).wantState("ready")

	if obj, ok := e.fake.Object("models/" + id); !ok || !bytes.Equal(obj, content) {
		t.Fatal("object in the bucket is missing or different")
	}
	if keys := e.fake.Keys(t); len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	if n := e.tempFiles(); n != 0 {
		t.Fatalf("%d temporary file(s) left behind", n)
	}

	dl := e.call("POST", "/api/v1/models/"+id+"/download", nil).wantState("ready")
	if dl.str("mode") != "proxy" || dl.has("url") || dl.num("size") != 4321 {
		t.Fatalf("download = %s", dl.body)
	}
	if got := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200).body; !bytes.Equal(got, content) {
		t.Fatal("content differs")
	}
	e.begin(id, len(content), nil).wantState("ready")
}

// S3 存储下的直连：客户端凭临时地址直接和对象存储收发内容。
func TestS3DirectFullFlow(t *testing.T) {
	e := newEnv(t, options{s3: true, transfer: "direct"})
	content, id := bundle(t, 2500)

	if got := e.call("GET", "/api/v1/ping", nil).want(200).str("transfer"); got != "direct" {
		t.Fatalf("transfer = %q", got)
	}

	begin := e.begin(id, len(content), nil).wantState("upload")
	uploadID := begin.str("upload_id")
	if begin.str("mode") != "direct" || begin.num("part_count") != 3 || begin.num("chunk_bytes") != 1000 {
		t.Fatalf("begin = %s", begin.body)
	}
	// 在直连会话上调中转的分片接口。
	e.putPart(uploadID, content, 1).wantError(409, "wrong_mode")

	// 进度按“已经签发到第几片”估算。
	if got := e.status(id).num("received"); got != 0 {
		t.Fatalf("received before any part = %d", got)
	}
	e.putDirect(uploadID, 1, e.part(content, 1))
	if got := e.status(id).num("received"); got != 0 {
		t.Fatalf("received after requesting part 1 = %d", got)
	}
	e.putDirect(uploadID, 2, e.part(content, 2))
	if got := e.status(id).num("received"); got != 1000 {
		t.Fatalf("received after requesting part 2 = %d", got)
	}
	e.putDirect(uploadID, 3, e.part(content, 3))
	st := e.status(id)
	if st.str("state") != "uploading" || st.num("received") != 2000 || st.num("size") != 2500 {
		t.Fatalf("status = %s", st.body)
	}
	// 重传一片（覆盖）。
	e.putDirect(uploadID, 2, e.part(content, 2))
	e.call("POST", "/api/v1/uploads/"+uploadID+"/parts/4/url", nil).wantError(400, "bad_part")

	// 核对通过之前，最终位置上什么都没有。
	if _, ok := e.fake.Object("models/" + id); ok {
		t.Fatal("object is at the final key before the upload was verified")
	}
	e.complete(uploadID).wantState("ready")
	if obj, ok := e.fake.Object("models/" + id); !ok || !bytes.Equal(obj, content) {
		t.Fatal("object in the bucket is missing or different")
	}
	if keys := e.fake.Keys(t); len(keys) != 1 {
		t.Fatalf("staging objects left behind: %v", keys)
	}

	// 下载：默认给临时地址；客户端可以要求改走中转。
	dl := e.call("POST", "/api/v1/models/"+id+"/download", nil).wantState("ready")
	if dl.str("mode") != "direct" || dl.num("size") != 2500 || dl.num("expires_in") != 900 {
		t.Fatalf("download = %s", dl.body)
	}
	got := e.do("GET", dl.str("url"), nil, nil).want(200)
	if !bytes.Equal(got.body, content) {
		t.Fatal("content from the direct url differs")
	}
	dl = e.call("POST", "/api/v1/models/"+id+"/download", map[string]any{"mode": "proxy"}).wantState("ready")
	if dl.str("mode") != "proxy" || dl.has("url") {
		t.Fatalf("download (proxy) = %s", dl.body)
	}
	if got := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200).body; !bytes.Equal(got, content) {
		t.Fatal("content from the file endpoint differs")
	}
	// 两次下载请求都算数。
	item := e.admin("GET", "/models", nil).fields()["items"].([]any)[0].(map[string]any)
	if item["download_count"] != float64(2) {
		t.Fatalf("download_count = %v", item["download_count"])
	}

	// 默认直连的服务器也接受客户端要求走中转的上传。
	other, otherID := bundle(t, 1700)
	fallback := e.begin(otherID, len(other), map[string]any{"mode": "proxy"}).wantState("upload")
	if fallback.str("mode") != "proxy" {
		t.Fatalf("fallback begin = %s", fallback.body)
	}
	fallbackID := fallback.str("upload_id")
	e.call("POST", "/api/v1/uploads/"+fallbackID+"/parts/1/url", nil).wantError(409, "wrong_mode")
	e.putPart(fallbackID, other, 1).want(200)
	e.putPart(fallbackID, other, 2).want(200)
	e.complete(fallbackID).wantState("ready")
	if obj, ok := e.fake.Object("models/" + otherID); !ok || !bytes.Equal(obj, other) {
		t.Fatal("object uploaded through the proxy fallback is missing or different")
	}
}

// 关掉直连上传的哈希核对之后：不再把内容读回来算哈希，但大小和文件头照样要对。
func TestDirectWithoutHashVerification(t *testing.T) {
	e := newEnv(t, options{s3: true, transfer: "direct", noVerifyDirect: true})
	content, id := bundle(t, 2200)
	e.mustUpload(id, content, nil)
	if got := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200).body; !bytes.Equal(got, content) {
		t.Fatal("content differs")
	}

	junk, _ := bundle(t, 1800)
	copy(junk, "NotUnity")
	junkID := idOf(junk)
	if r := e.complete(e.upload(junkID, junk, nil)).wantState("rejected"); r.str("reason") != "not_a_bundle" {
		t.Fatalf("rejected = %s", r.body)
	}
	e.wantNotDownloadable(junkID, "missing")
}

// 对象存储临时出错时，complete 报 5xx 但会话保留，再调一次就能成功。
func TestCompleteCanBeRetriedAfterStorageError(t *testing.T) {
	t.Run("proxy", func(t *testing.T) {
		e := newEnv(t, options{s3: true})
		content, id := bundle(t, 2500)
		uploadID := e.upload(id, content, nil)
		// 放进存储的 PUT 连续失败（含内部的一次重试）。
		e.fake.FailNext("PUT", "/models/"+id, 2)
		e.complete(uploadID).wantError(500, "internal")
		if got := e.status(id).str("state"); got != "uploading" {
			t.Fatalf("state after failed complete = %q", got)
		}
		e.complete(uploadID).wantState("ready")
		if obj, ok := e.fake.Object("models/" + id); !ok || !bytes.Equal(obj, content) {
			t.Fatal("object missing or different")
		}
	})
	t.Run("direct", func(t *testing.T) {
		e := newEnv(t, options{s3: true, transfer: "direct"})
		content, id := bundle(t, 2500)
		uploadID := e.upload(id, content, nil)
		// 分片已经拼好、核对也过了，最后复制到最终位置时失败。
		e.fake.FailNext("PUT", "/models/"+id, 1)
		e.complete(uploadID).wantError(500, "internal")
		if _, ok := e.fake.Object("models/" + id); ok {
			t.Fatal("object must not be at the final key after a failed complete")
		}
		e.complete(uploadID).wantState("ready")
		if obj, ok := e.fake.Object("models/" + id); !ok || !bytes.Equal(obj, content) {
			t.Fatal("object missing or different")
		}
		if keys := e.fake.Keys(t); len(keys) != 1 {
			t.Fatalf("keys = %v", keys)
		}
	})
}

// 上传会话空闲超时后作废，id 回到 missing。
func TestUploadSessionTimeout(t *testing.T) {
	t.Run("proxy", func(t *testing.T) {
		e := newEnv(t, options{})
		content, id := bundle(t, 2500)
		uploadID := e.begin(id, len(content), nil).wantState("upload").str("upload_id")

		e.clock.Advance(100 * time.Second)
		e.putPart(uploadID, content, 1).want(200) // 有字节流入，重新计时
		e.clock.Advance(119 * time.Second)
		e.app.SweepUploads()
		if got := e.status(id).str("state"); got != "uploading" {
			t.Fatalf("state before the timeout = %q", got)
		}

		e.clock.Advance(2 * time.Second)
		e.app.SweepUploads()
		if got := e.status(id).str("state"); got != "missing" {
			t.Fatalf("state after the timeout = %q", got)
		}
		e.putPart(uploadID, content, 2).wantError(404, "upload_gone")
		e.complete(uploadID).wantError(404, "upload_gone")
		if n := e.tempFiles(); n != 0 {
			t.Fatalf("%d temporary file(s) left behind", n)
		}
		// 之后可以重新开始。
		e.mustUpload(id, content, nil)
	})

	t.Run("direct", func(t *testing.T) {
		e := newEnv(t, options{s3: true, transfer: "direct"})
		content, id := bundle(t, 2500)
		uploadID := e.begin(id, len(content), nil).wantState("upload").str("upload_id")

		e.clock.Advance(800 * time.Second)
		e.putDirect(uploadID, 1, e.part(content, 1)) // 接口调用，重新计时
		e.clock.Advance(899 * time.Second)
		e.app.SweepUploads()
		if got := e.status(id).str("state"); got != "uploading" {
			t.Fatalf("state before the timeout = %q", got)
		}

		e.clock.Advance(2 * time.Second)
		e.app.SweepUploads()
		if got := e.status(id).str("state"); got != "missing" {
			t.Fatalf("state after the timeout = %q", got)
		}
		e.call("POST", "/api/v1/uploads/"+uploadID+"/parts/2/url", nil).wantError(404, "upload_gone")
		e.complete(uploadID).wantError(404, "upload_gone")

		// 对象存储上没完成的分片上传被中止了。
		aborted := false
		for _, req := range e.fake.Requests() {
			if strings.HasPrefix(req, "DELETE /"+s3test.Bucket+"/models/_incoming/"+uploadID+"?uploadId=") {
				aborted = true
			}
		}
		if !aborted {
			t.Fatalf("multipart upload was not aborted; requests: %v", e.fake.Requests())
		}
		e.mustUpload(id, content, nil)
	})
}

// abort：会话作废，临时数据清掉；会话不存在也返回 200。
func TestAbort(t *testing.T) {
	e := newEnv(t, options{})
	content, id := bundle(t, 2500)
	uploadID := e.upload(id, content, nil, 3)

	r := e.call("POST", "/api/v1/uploads/"+uploadID+"/abort", nil).want(200)
	if r.fields()["ok"] != true {
		t.Fatalf("abort = %s", r.body)
	}
	if got := e.status(id).str("state"); got != "missing" {
		t.Fatalf("state after abort = %q", got)
	}
	if n := e.tempFiles(); n != 0 {
		t.Fatalf("%d temporary file(s) left behind", n)
	}
	e.call("POST", "/api/v1/uploads/"+uploadID+"/abort", nil).want(200)
	e.call("POST", "/api/v1/uploads/ffffffffffffffffffffffffffffffff/abort", nil).want(200)
	e.call("POST", "/api/v1/uploads/not-an-id/abort", nil).want(200)
}

// Range 续传。
func TestFileRange(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts options
	}{
		{"local", options{}},
		{"s3", options{s3: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts)
			content, id := bundle(t, 3210)
			e.mustUpload(id, content, nil)
			path := "/api/v1/models/" + id + "/file"
			get := func(rng string) response {
				return e.do("GET", path, nil, map[string]string{"Authorization": "Bearer " + testAuthKey, "Range": rng})
			}

			// 断点续传：从第 N 个字节到结尾。
			r := get("bytes=1234-").want(206)
			if !bytes.Equal(r.body, content[1234:]) || r.header.Get("Content-Range") != "bytes 1234-3209/3210" ||
				r.header.Get("Content-Length") != "1976" || r.header.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("bytes=1234-: %d bytes, headers %v", len(r.body), r.header)
			}
			r = get("bytes=0-").want(206)
			if !bytes.Equal(r.body, content) || r.header.Get("Content-Range") != "bytes 0-3209/3210" {
				t.Fatalf("bytes=0-: %d bytes, headers %v", len(r.body), r.header)
			}
			r = get("bytes=10-19").want(206)
			if !bytes.Equal(r.body, content[10:20]) || r.header.Get("Content-Range") != "bytes 10-19/3210" {
				t.Fatalf("bytes=10-19: %d bytes, headers %v", len(r.body), r.header)
			}
			// 结尾超出文件：截到文件末尾。
			r = get("bytes=3200-99999").want(206)
			if !bytes.Equal(r.body, content[3200:]) || r.header.Get("Content-Range") != "bytes 3200-3209/3210" {
				t.Fatalf("bytes=3200-99999: %d bytes, headers %v", len(r.body), r.header)
			}
			r = get("bytes=-10").want(206)
			if !bytes.Equal(r.body, content[3200:]) {
				t.Fatalf("bytes=-10: %d bytes", len(r.body))
			}
			// 起点在文件之外。
			r = get("bytes=3210-").want(416)
			if r.header.Get("Content-Range") != "bytes */3210" {
				t.Fatalf("416 headers = %v", r.header)
			}
			// 看不懂的写法当作没带。
			if r := get("bytes=5-1,7-9").want(200); !bytes.Equal(r.body, content) {
				t.Fatal("multi-range should fall back to the whole file")
			}
			if r := get("lines=1-2").want(200); !bytes.Equal(r.body, content) {
				t.Fatal("unknown unit should fall back to the whole file")
			}

			head := e.do("HEAD", path, nil, map[string]string{"Authorization": "Bearer " + testAuthKey}).want(200)
			if head.header.Get("Content-Length") != "3210" || len(head.body) != 0 || head.header.Get("Accept-Ranges") != "bytes" {
				t.Fatalf("HEAD headers = %v", head.header)
			}
		})
	}
}

// 请求格式不对的各种情况。
func TestBadRequests(t *testing.T) {
	e := newEnv(t, options{})
	content, id := bundle(t, 2500)

	for _, bad := range []string{"xyz", strings.ToUpper(id), id + "0", id[:31], id[:31] + "g"} {
		e.call("GET", "/api/v1/models/"+bad, nil).wantError(400, "bad_id")
		e.call("POST", "/api/v1/models/"+bad+"/upload", map[string]any{"size": 1}).wantError(400, "bad_id")
		e.call("POST", "/api/v1/models/"+bad+"/download", nil).wantError(400, "bad_id")
		e.call("GET", "/api/v1/models/"+bad+"/file", nil).wantError(400, "bad_id")
	}

	up := "/api/v1/models/" + id + "/upload"
	e.call("POST", up, nil).wantError(400, "bad_request")
	e.call("POST", up, []byte("{not json")).wantError(400, "bad_request")
	e.call("POST", up, map[string]any{"name": "x"}).wantError(400, "bad_request")
	e.call("POST", up, map[string]any{"size": 0}).wantError(400, "bad_request")
	e.call("POST", up, map[string]any{"size": -5}).wantError(400, "bad_request")
	e.call("POST", up, map[string]any{"size": "12"}).wantError(400, "bad_request")
	e.call("POST", up, map[string]any{"size": 10, "name": strings.Repeat("x", 70_000)}).wantError(413, "too_large")
	e.call("POST", "/api/v1/models/"+id+"/download", []byte("[1,2")).wantError(400, "bad_request")

	uploadID := e.begin(id, len(content), nil).wantState("upload").str("upload_id")
	parts := "/api/v1/uploads/" + uploadID + "/parts/"
	e.call("PUT", parts+"0", content[:1000]).wantError(400, "bad_part")
	e.call("PUT", parts+"4", content[:1000]).wantError(400, "bad_part")
	e.call("PUT", parts+"abc", content[:1000]).wantError(400, "bad_part")
	e.call("PUT", parts+"1", content[:999]).wantError(400, "bad_part")  // 太短
	e.call("PUT", parts+"3", content[:1000]).wantError(400, "bad_part") // 最后一片应该是 500
	e.call("PUT", parts+"1", content[:1001]).wantError(413, "too_large")
	e.call("PUT", parts+"1", nil).wantError(400, "bad_part")
	// 出过错的会话照样能用。
	e.putPart(uploadID, content, 1).want(200)

	e.call("PUT", "/api/v1/uploads/ffffffffffffffffffffffffffffffff/parts/1", content[:1000]).wantError(404, "upload_gone")
	e.call("POST", "/api/v1/uploads/ffffffffffffffffffffffffffffffff/complete", nil).wantError(404, "upload_gone")
	e.call("POST", "/api/v1/uploads/nonsense/complete", nil).wantError(404, "upload_gone")
	e.call("POST", "/api/v1/uploads/ffffffffffffffffffffffffffffffff/parts/1/url", nil).wantError(404, "upload_gone")

	e.call("GET", "/api/v1/nope", nil).wantError(404, "not_found")
	e.call("GET", "/api/v1/models/"+id+"/nope", nil).wantError(404, "not_found")
	e.call("GET", "/api/v1/ping/", nil).wantError(404, "not_found")
	e.call("GET", "/nope", nil).wantError(404, "not_found")
	if r := e.call("POST", "/api/v1/ping", nil).wantError(405, "method_not_allowed"); r.header.Get("Allow") != "GET" {
		t.Fatalf("Allow = %q", r.header.Get("Allow"))
	}
	e.call("DELETE", "/api/v1/models/"+id, nil).wantError(405, "method_not_allowed")
}

// 客户端密钥不带或不对：一律 401，而且探不出哪些地址存在。
func TestClientAuth(t *testing.T) {
	e := newEnv(t, options{})
	_, id := bundle(t, 100)

	for _, auth := range []string{
		"",
		"Bearer",
		"Bearer ",
		"Bearer wrong-key",
		"Bearer " + testAuthKey + "x",
		"Bearer " + testAuthKey[:len(testAuthKey)-1],
		"Bearer " + testAdminPassword, // 管理密码不能当客户端密钥用
		"Basic " + testAuthKey,
		testAuthKey,
	} {
		headers := map[string]string{}
		if auth != "" {
			headers["Authorization"] = auth
		}
		for _, req := range [][2]string{
			{"GET", "/api/v1/ping"},
			{"GET", "/api/v1/models/" + id},
			{"POST", "/api/v1/models/" + id + "/upload"},
			{"POST", "/api/v1/models/" + id + "/download"},
			{"GET", "/api/v1/models/" + id + "/file"},
			{"PUT", "/api/v1/uploads/ffffffffffffffffffffffffffffffff/parts/1"},
			{"POST", "/api/v1/uploads/ffffffffffffffffffffffffffffffff/complete"},
			{"GET", "/api/v1/does-not-exist"},
			{"GET", "/api/v1/models/bad-id"},
		} {
			r := e.do(req[0], req[1], nil, headers).wantError(401, "unauthorized")
			if r.header.Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control missing on 401")
			}
		}
	}
	// 管理员会话也不能当客户端密钥用。
	e.login()
	e.do("GET", "/api/v1/ping", nil, map[string]string{"Cookie": "vml_admin=" + e.adminCookie.Value}).wantError(401, "unauthorized")
	// 密钥对了就行（Bearer 不区分大小写）。
	e.do("GET", "/api/v1/ping", nil, map[string]string{"Authorization": "bearer " + testAuthKey}).want(200)
}

// base_path 之外一律 404，不暴露任何信息。
func TestOutsideBasePath(t *testing.T) {
	e := newEnv(t, options{basePath: "/vml-3f9a1c"})
	auth := map[string]string{"Authorization": "Bearer " + testAuthKey}

	e.do("GET", e.srv.URL+"/vml-3f9a1c/api/v1/ping", nil, auth).want(200)

	for _, path := range []string{
		"/",
		"/api/v1/ping",
		"/admin",
		"/admin/",
		"/api/v1/admin/session",
		"/vml-3f9a1",
		"/vml-3f9a1cx/api/v1/ping",
		"/vml-3f9a1c.bak/api/v1/ping",
		"/VML-3F9A1C/api/v1/ping",
		"/x/vml-3f9a1c/api/v1/ping",
		"/favicon.ico",
	} {
		for _, method := range []string{"GET", "POST"} {
			r := e.do(method, e.srv.URL+path, nil, auth).want(404)
			// 和一个空服务器一样：没有 JSON、没有任何能认出这是什么服务的东西。
			if strings.Contains(string(r.body), "{") || strings.Contains(strings.ToLower(string(r.body)), "vml") ||
				r.header.Get("WWW-Authenticate") != "" || r.header.Get("Set-Cookie") != "" {
				t.Fatalf("%s %s leaks information: %q %v", method, path, r.body, r.header)
			}
		}
	}

	// base_path 之内：没有的地址是 JSON 的 404，接口照常鉴权。
	e.do("GET", e.srv.URL+"/vml-3f9a1c", nil, auth).wantError(404, "not_found")
	e.do("GET", e.srv.URL+"/vml-3f9a1c/", nil, auth).wantError(404, "not_found")
	e.do("GET", e.srv.URL+"/vml-3f9a1c/api/v1/ping", nil, nil).wantError(401, "unauthorized")

	// 管理页和登录 Cookie 的路径都跟着 base_path 走。
	r := e.do("GET", e.srv.URL+"/vml-3f9a1c/admin", nil, nil).want(302)
	if r.header.Get("Location") != "admin/" {
		t.Fatalf("Location = %q", r.header.Get("Location"))
	}
	e.do("GET", e.srv.URL+"/vml-3f9a1c/admin/", nil, nil).want(200)
	login := e.do("POST", "/api/v1/admin/login", map[string]any{"password": testAdminPassword}, nil).want(200)
	if c := login.header.Get("Set-Cookie"); !strings.Contains(c, "Path=/vml-3f9a1c/;") {
		t.Fatalf("Set-Cookie = %q", c)
	}

	// 整个流程在 base_path 下照常工作。
	content, id := bundle(t, 1500)
	e.mustUpload(id, content, nil)
	if got := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200).body; !bytes.Equal(got, content) {
		t.Fatal("content differs")
	}
}

// 文件名和昵称：截断超长的，去掉控制字符。
func TestNamesAreCleaned(t *testing.T) {
	e := newEnv(t, options{})
	content, id := bundle(t, 500)
	name := "evil\u0000name\r\n\t‮gnp.vrca" + strings.Repeat("长", 300)
	uploader := "  nick\u0007name" + strings.Repeat("y", 100)
	e.mustUpload(id, content, map[string]any{"name": name, "uploader": uploader})

	item := e.admin("GET", "/models", nil).fields()["items"].([]any)[0].(map[string]any)
	gotName, gotUploader := item["name"].(string), item["uploader"].(string)
	if !strings.HasPrefix(gotName, "evilnamegnp.vrca长") || len([]rune(gotName)) != 200 {
		t.Fatalf("name = %q (%d chars)", gotName, len([]rune(gotName)))
	}
	if !strings.HasPrefix(gotUploader, "nickname") || len([]rune(gotUploader)) != 64 {
		t.Fatalf("uploader = %q (%d chars)", gotUploader, len([]rune(gotUploader)))
	}
}

// 管理页的静态文件。
func TestAdminPage(t *testing.T) {
	e := newEnv(t, options{})

	r := e.do("GET", "/admin", nil, nil).want(302)
	if r.header.Get("Location") != "admin/" {
		t.Fatalf("Location = %q", r.header.Get("Location"))
	}
	page := e.do("GET", "/admin/", nil, nil).want(200)
	if !strings.Contains(string(page.body), "<title>admin</title>") ||
		!strings.HasPrefix(page.header.Get("Content-Type"), "text/html") ||
		page.header.Get("Cache-Control") != "no-store" ||
		page.header.Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(page.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("admin page: %q %v", page.body, page.header)
	}
	// 脚本只能来自本站的文件：不允许内联脚本，也不允许任何外部来源。
	if csp := page.header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self';") ||
		!strings.Contains(csp, "default-src 'self';") || strings.Contains(csp, "http") {
		t.Fatalf("Content-Security-Policy = %q", csp)
	}
	if js := e.do("GET", "/admin/app.js", nil, nil).want(200); !strings.HasPrefix(js.header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("app.js Content-Type = %q", js.header.Get("Content-Type"))
	}
	if css := e.do("GET", "/admin/css/app.css", nil, nil).want(200); !strings.HasPrefix(css.header.Get("Content-Type"), "text/css") {
		t.Fatalf("app.css Content-Type = %q", css.header.Get("Content-Type"))
	}
	for _, path := range []string{"/admin/nope.js", "/admin/css", "/admin/css/", "/admin/.hidden", "/admin/../go.mod", "/admin//app.js"} {
		e.do("GET", path, nil, nil).want(404)
	}
	e.do("POST", "/admin/", nil, nil).want(405)
}

// 文件大小正好是分片的整数倍、只有一片、只有一个字节多出来：分片数都要算对。
func TestPartCountEdges(t *testing.T) {
	e := newEnv(t, options{})
	for _, size := range []int{7, 999, 1000, 1001, 2000, 3001} {
		content, id := bundle(t, size)
		r := e.begin(id, size, nil).wantState("upload")
		want := (size + 999) / 1000
		if int(r.num("part_count")) != want {
			t.Fatalf("size %d: part_count = %d, want %d", size, r.num("part_count"), want)
		}
		for n := 1; n <= want; n++ {
			e.putPart(r.str("upload_id"), content, n).want(200)
		}
		e.complete(r.str("upload_id")).wantState("ready")
		if got := e.call("GET", "/api/v1/models/"+id+"/file", nil).want(200); !bytes.Equal(got.body, content) ||
			got.header.Get("Content-Length") != strconv.Itoa(size) {
			t.Fatalf("size %d: downloaded content differs", size)
		}
	}
}
