package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"vml-sync-server/internal/meta"
	"vml-sync-server/internal/s3test"
)

func openMeta(t *testing.T, dataDir string) *meta.DB {
	t.Helper()
	db, err := meta.Open(filepath.Join(dataDir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// 换存储（docs/API.md 3.5）：要确认、先验证、然后清空并切换；只换凭据不清空。
func TestStorageChange(t *testing.T) {
	e := newEnv(t, options{})
	e.fake = s3test.New(t)

	a, aID := bundle(t, 2500)
	e.mustUpload(aID, a, map[string]any{"name": "Kept.vrca"})
	b, bID := bundle(t, 1200)
	e.mustUpload(bID, b, nil)
	_, blockedID := bundle(t, 10)
	e.admin("POST", "/blocklist", map[string]any{"id": blockedID, "note": "stays"}).want(200)
	c, cID := bundle(t, 2500)
	inFlight := e.upload(cID, c, nil, 3)

	before := e.settingsOf()
	unchanged := func(step string) {
		t.Helper()
		if got := e.settingsOf(); describe(got) != describe(before) {
			t.Fatalf("%s: settings changed:\n%s", step, describe(got))
		}
		e.wantDownloadable(aID, a)
		e.wantDownloadable(bID, b)
		if got := e.status(cID).str("state"); got != "uploading" {
			t.Fatalf("%s: the upload in progress is now %q", step, got)
		}
		if total := e.admin("GET", "/models", nil).num("total"); total != 2 {
			t.Fatalf("%s: %d models", step, total)
		}
		if keys := e.fake.Keys(t); len(keys) != 0 {
			t.Fatalf("%s: objects appeared in the new storage: %v", step, keys)
		}
	}
	good := e.s3Settings(s3test.AccessKey, s3test.SecretKey)

	// 1. 不带确认：409，告诉页面现在有几个模型，什么都不改。
	for _, body := range []map[string]any{
		{"storage": good},
		{"storage": good, "confirm_storage_change": false},
	} {
		r := e.admin("PUT", "/settings", body).wantError(409, "storage_change_needs_confirm")
		if r.num("model_count") != 2 || r.str("message") == "" {
			t.Fatalf("409 = %s", r.body)
		}
	}
	unchanged("without confirmation")

	// 2. 带了确认，但新存储用不了：400，什么都不改；message 看得出是哪种问题。
	for want, storage := range map[string]map[string]any{
		"credentials":  e.s3Settings(s3test.AccessKey, "not-the-secret"),
		"credentials ": e.s3Settings("nobody", s3test.SecretKey),
		"bucket":       {"type": "s3", "s3": map[string]any{"endpoint": e.fake.URL, "bucket": "no-such-bucket", "access_key_id": s3test.AccessKey, "secret_access_key": s3test.SecretKey}},
		"cannot reach": {"type": "s3", "s3": map[string]any{"endpoint": "http://127.0.0.1:1", "bucket": s3test.Bucket, "access_key_id": s3test.AccessKey, "secret_access_key": s3test.SecretKey}},
	} {
		r := e.admin("PUT", "/settings", map[string]any{"storage": storage, "confirm_storage_change": true}).wantError(400, "storage_unreachable")
		if !strings.Contains(r.str("message"), strings.TrimSpace(want)) {
			t.Fatalf("storage_unreachable message %q does not mention %q", r.str("message"), want)
		}
		assertNoSecret(t, r, s3test.SecretKey, "not-the-secret")
	}
	unchanged("with an unusable storage")

	// 3. 带确认、新存储能用：清空并切换。
	r := e.admin("PUT", "/settings", map[string]any{"storage": good, "confirm_storage_change": true}).want(200)
	storage := r.fields()["storage"].(map[string]any)
	s3 := storage["s3"].(map[string]any)
	if storage["type"] != "s3" || s3["endpoint"] != e.fake.URL || s3["bucket"] != s3test.Bucket ||
		s3["access_key_id"] != s3test.AccessKey || s3["secret_access_key_set"] != true {
		t.Fatalf("settings after the change = %s", r.body)
	}
	// secret_access_key 只写不读：改完的响应和之后的 GET 里都没有它。
	assertNoSecret(t, r, s3test.SecretKey)
	assertNoSecret(t, e.admin("GET", "/settings", nil).want(200), s3test.SecretKey)

	// 模型清空了；旧存储里的对象也删了。
	if total := e.admin("GET", "/models", nil).num("total"); total != 0 {
		t.Fatalf("%d models left after the storage change", total)
	}
	e.wantNotDownloadable(aID, "missing")
	e.wantNotDownloadable(bID, "missing")
	if n := e.localObjects(); n != 0 {
		t.Fatalf("%d object(s) left in the old storage", n)
	}
	// 禁止名单保留。
	list := e.admin("GET", "/blocklist", nil).want(200).fields()["items"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != blockedID || list[0].(map[string]any)["note"] != "stays" {
		t.Fatalf("blocklist after the storage change = %v", list)
	}
	e.wantNotDownloadable(blockedID, "blocked")
	// 进行中的上传作废了。
	e.putPart(inFlight, c, 3).wantError(404, "upload_gone")
	e.complete(inFlight).wantError(404, "upload_gone")
	if got := e.status(cID).str("state"); got != "missing" {
		t.Fatalf("state of the cancelled upload = %q", got)
	}
	if n := e.tempFiles(); n != 0 {
		t.Fatalf("%d temporary file(s) left behind", n)
	}
	ov := e.admin("GET", "/overview", nil).want(200)
	if ov.str("storage") != "s3" || ov.num("model_count") != 0 || ov.num("total_bytes") != 0 || ov.num("active_uploads") != 0 || ov.num("blocked_count") != 1 {
		t.Fatalf("overview = %s", ov.body)
	}
	// 之后有人需要时再传上来：进的是新存储。
	e.mustUpload(aID, a, nil)
	if obj, ok := e.fake.Object("models/" + aID); !ok || !bytes.Equal(obj, a) {
		t.Fatal("re-uploaded model is not in the new storage")
	}
	if n := e.localObjects(); n != 0 {
		t.Fatalf("re-uploaded model went to the old storage (%d local object(s))", n)
	}

	// 4. 只换凭据（位置没变）：不用确认，验证能用之后直接生效，不清空。
	e.fake.SetKey("second-access-key", "second-secret-key")
	r = e.putSettings(map[string]any{"storage": map[string]any{"s3": map[string]any{
		"access_key_id": "second-access-key", "secret_access_key": "second-secret-key",
	}}})
	if got := r.fields()["storage"].(map[string]any)["s3"].(map[string]any)["access_key_id"]; got != "second-access-key" {
		t.Fatalf("access_key_id = %v", got)
	}
	assertNoSecret(t, r, "second-secret-key")
	e.fake.RemoveKey(s3test.AccessKey) // 旧凭据作废了也不要紧：现在用的是新的
	e.wantDownloadable(aID, a)
	e.mustUpload(bID, b, nil)
	// 凭据不对：不保存。
	e.admin("PUT", "/settings", map[string]any{"storage": map[string]any{"s3": map[string]any{"secret_access_key": "typo"}}}).wantError(400, "storage_unreachable")
	e.wantDownloadable(aID, a)
	// secret_access_key 不带或给空串 = 保持原来的。
	e.putSettings(map[string]any{"storage": map[string]any{"s3": map[string]any{"access_key_id": "second-access-key", "secret_access_key": ""}}})
	e.putSettings(map[string]any{"storage": map[string]any{"type": "s3", "s3": map[string]any{"region": s3test.Region, "path_style": true}}})
	e.putSettings(map[string]any{"retention_hours": 10})
	e.wantDownloadable(aID, a)
	e.wantDownloadable(bID, b)

	// 5. 位置变了（哪怕只是前缀）：又是换存储。
	moved := map[string]any{"storage": map[string]any{"s3": map[string]any{"prefix": "elsewhere/"}}}
	if r := e.admin("PUT", "/settings", moved).wantError(409, "storage_change_needs_confirm"); r.num("model_count") != 2 {
		t.Fatalf("409 = %s", r.body)
	}
	moved["confirm_storage_change"] = true
	e.putSettings(moved)
	if keys := e.fake.Keys(t); len(keys) != 0 {
		t.Fatalf("objects left under the old prefix: %v", keys)
	}
	e.mustUpload(aID, a, nil)
	if _, ok := e.fake.Object("elsewhere/" + aID); !ok {
		t.Fatalf("model is not under the new prefix: %v", e.fake.Keys(t))
	}

	// 6. 换回本地磁盘：同样要确认，同样清空；保存过的 S3 凭据留着备用。
	e.admin("PUT", "/settings", map[string]any{"storage": map[string]any{"type": "local"}}).wantError(409, "storage_change_needs_confirm")
	r = e.putSettings(map[string]any{"storage": map[string]any{"type": "local"}, "confirm_storage_change": true})
	storage = r.fields()["storage"].(map[string]any)
	if storage["type"] != "local" || storage["s3"].(map[string]any)["secret_access_key_set"] != true {
		t.Fatalf("settings after going back to local = %s", r.body)
	}
	if keys := e.fake.Keys(t); len(keys) != 0 {
		t.Fatalf("objects left in the bucket: %v", keys)
	}
	e.wantNotDownloadable(aID, "missing")
	e.mustUpload(aID, a, nil)
	if n := e.localObjects(); n != 1 {
		t.Fatalf("%d local object(s)", n)
	}

	// 重启之后：设置、模型、禁止名单都还在。
	e = e.restart()
	if got := e.settingsOf()["storage"].(map[string]any); got["type"] != "local" {
		t.Fatalf("storage after restart = %v", got)
	}
	e.wantDownloadable(aID, a)
	e.wantNotDownloadable(blockedID, "blocked")
}

// 换存储的时候正好有上传在收尾：它传完的东西不会登记到新存储名下，也不会留在旧存储里。
func TestStorageChangeDuringComplete(t *testing.T) {
	e := newEnv(t, options{s3: true})
	content, id := bundle(t, 2500)
	uploadID := e.upload(id, content, nil)

	// 让“放进对象存储”这一步先失败一次，这样会话停在“核对通过、等着重试”的状态。
	e.fake.FailNext("PUT", "/models/"+id, 2)
	e.complete(uploadID).wantError(500, "internal")

	e.putSettings(map[string]any{"storage": map[string]any{"type": "local"}, "confirm_storage_change": true})

	// 旧会话已经作废；模型不存在；两边的存储里都没有它。
	e.complete(uploadID).wantError(404, "upload_gone")
	e.wantNotDownloadable(id, "missing")
	if keys := e.fake.Keys(t); len(keys) != 0 {
		t.Fatalf("bucket = %v", keys)
	}
	if n := e.localObjects(); n != 0 {
		t.Fatalf("%d local object(s)", n)
	}
	e.mustUpload(id, content, nil)
	e.wantDownloadable(id, content)
}

// 同上，但这次上传真的正卡在“往旧存储里放”的半路上：换完存储之后它才放完。
// 它不能登记成功，放进旧存储的对象也要撤回来。
func TestStorageChangeRacesWithComplete(t *testing.T) {
	e := newEnv(t, options{s3: true})
	content, id := bundle(t, 2500)
	uploadID := e.upload(id, content, nil)

	arrived, release := e.fake.Hold("PUT", "/models/"+id)
	defer release()
	done := make(chan response, 1)
	go func() { done <- e.complete(uploadID) }()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("complete never reached the storage")
	}

	// 这时换存储：不用等那个上传。
	e.putSettings(map[string]any{"storage": map[string]any{"type": "local"}, "confirm_storage_change": true})
	if total := e.admin("GET", "/models", nil).num("total"); total != 0 {
		t.Fatalf("%d models right after the storage change", total)
	}

	// 放行：旧存储收下了对象，但服务器发现存储已经换了。
	release()
	select {
	case r := <-done:
		r.wantError(404, "upload_gone")
	case <-time.After(10 * time.Second):
		t.Fatal("complete did not return")
	}
	e.wantNotDownloadable(id, "missing")
	if total := e.admin("GET", "/models", nil).num("total"); total != 0 {
		t.Fatalf("%d models after the late upload finished", total)
	}
	if keys := e.fake.Keys(t); len(keys) != 0 {
		t.Fatalf("the late upload left objects in the old storage: %v", keys)
	}
	if n := e.localObjects(); n != 0 {
		t.Fatalf("%d local object(s)", n)
	}
	if n := e.tempFiles(); n != 0 {
		t.Fatalf("%d temporary file(s) left behind", n)
	}
	// 之后照常能传。
	e.mustUpload(id, content, nil)
	e.wantDownloadable(id, content)
}

// 设置里的存储不可用：程序照常启动，管理页照常能进，客户端的上传下载接口返回 503；恢复后自动恢复。
func TestStorageUnavailable(t *testing.T) {
	e := newEnv(t, options{s3: true, retryInterval: 20 * time.Millisecond})
	content, id := bundle(t, 2500)
	e.mustUpload(id, content, nil)

	// 存储那边的凭据被吊销了，然后服务重启。
	e.fake.RemoveKey(s3test.AccessKey)
	e = e.restart()
	if ready, reason := e.app.StorageReady(); ready || !strings.Contains(reason, "credentials") {
		t.Fatalf("StorageReady = %v, %q", ready, reason)
	}

	// 客户端：探测照常（地址和密钥是对的），上传下载相关的一律 503。
	e.call("GET", "/api/v1/ping", nil).want(200)
	other, otherID := bundle(t, 1500)
	const someUpload = "ffffffffffffffffffffffffffffffff"
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/models/" + id, nil},
		{"POST", "/api/v1/models/" + otherID + "/upload", map[string]any{"size": len(other)}},
		{"POST", "/api/v1/models/" + id + "/download", nil},
		{"GET", "/api/v1/models/" + id + "/file", nil},
		{"PUT", "/api/v1/uploads/" + someUpload + "/parts/1", other[:1000]},
		{"POST", "/api/v1/uploads/" + someUpload + "/parts/1/url", nil},
		{"POST", "/api/v1/uploads/" + someUpload + "/complete", nil},
	} {
		if r := e.call(req.method, req.path, req.body).wantError(503, "storage_unavailable"); r.str("message") == "" {
			t.Fatalf("503 without a message: %s", r.body)
		}
	}
	e.call("POST", "/api/v1/uploads/"+someUpload+"/abort", nil).want(200)
	// 密钥不对还是 401，不会因为存储坏了就把别的信息漏出去。
	e.do("GET", "/api/v1/models/"+id, nil, map[string]string{"Authorization": "Bearer nope"}).wantError(401, "unauthorized")

	// 管理页照常能进，能看、能改设置；要动存储的操作同样是 503。
	e.do("GET", "/admin/", nil, nil).want(200)
	e.login()
	if ov := e.admin("GET", "/overview", nil).want(200); ov.num("model_count") != 1 || ov.str("storage") != "s3" {
		t.Fatalf("overview = %s", ov.body)
	}
	e.admin("GET", "/models", nil).want(200)
	e.admin("GET", "/blocklist", nil).want(200)
	e.admin("GET", "/uploads", nil).want(200)
	e.admin("GET", "/settings", nil).want(200)
	e.admin("POST", "/models/delete", map[string]any{"ids": []string{id}}).wantError(503, "storage_unavailable")
	e.admin("POST", "/blocklist", map[string]any{"id": otherID}).wantError(503, "storage_unavailable")
	e.admin("POST", "/cleanup", nil).wantError(503, "storage_unavailable")
	// 测试存储设置能告诉管理员问题出在哪。
	if r := e.admin("POST", "/storage/test", map[string]any{}).want(200); r.fields()["ok"] != false || !strings.Contains(r.str("message"), "credentials") {
		t.Fatalf("storage test = %s", r.body)
	}
	// 和存储无关的设置照样能改。
	e.putSettings(map[string]any{"retention_hours": 100})
	e.call("GET", "/api/v1/models/"+id, nil).wantError(503, "storage_unavailable")

	// 把设置改对（换上一组有效的凭据）：立刻恢复，之前的模型还在。
	e.fake.SetKey("second-access-key", "second-secret-key")
	e.putSettings(map[string]any{"storage": map[string]any{"s3": map[string]any{
		"access_key_id": "second-access-key", "secret_access_key": "second-secret-key",
	}}})
	if ready, _ := e.app.StorageReady(); !ready {
		t.Fatal("storage should be usable after fixing the credentials")
	}
	e.wantDownloadable(id, content)
	e.mustUpload(otherID, other, nil)
	e.admin("POST", "/cleanup", nil).want(200)

	// 存储那边临时出了故障（设置没问题）：恢复之后后台自己重试，不用人管。
	e.fake.SetDown(true)
	e = e.restart()
	e.call("GET", "/api/v1/models/"+id, nil).wantError(503, "storage_unavailable")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.app.Run(ctx)
	time.Sleep(60 * time.Millisecond) // 故障期间重试了几次，都不成
	e.call("GET", "/api/v1/models/"+id, nil).wantError(503, "storage_unavailable")
	e.fake.SetDown(false)
	eventually(t, "the storage to come back by itself", func() bool {
		return e.call("GET", "/api/v1/models/"+id, nil).status == 200
	})
	e.wantDownloadable(id, content)
	e.wantDownloadable(otherID, other)
}

// 测试存储设置（docs/API.md 3.8）：只验证，不保存。
func TestStorageTestEndpoint(t *testing.T) {
	e := newEnv(t, options{})
	e.fake = s3test.New(t)
	before := e.settingsOf()
	test := func(storage any) response {
		t.Helper()
		return e.admin("POST", "/storage/test", map[string]any{"storage": storage}).want(200)
	}
	wantFailure := func(storage any, mention string) {
		t.Helper()
		r := test(storage)
		if r.fields()["ok"] != false || !strings.Contains(r.str("message"), mention) {
			t.Fatalf("storage test = %s, want a failure mentioning %q", r.body, mention)
		}
		assertNoSecret(t, r, s3test.SecretKey)
	}
	s3 := func(overrides map[string]any) map[string]any {
		fields := map[string]any{
			"endpoint": e.fake.URL, "region": s3test.Region, "bucket": s3test.Bucket,
			"access_key_id": s3test.AccessKey, "secret_access_key": s3test.SecretKey,
		}
		for k, v := range overrides {
			fields[k] = v
		}
		return map[string]any{"type": "s3", "s3": fields}
	}

	// 能用：就一个 ok。
	if r := test(s3(nil)); string(r.body) != `{"ok":true}` {
		t.Fatalf("storage test = %s", r.body)
	}
	// 本地磁盘、以及“不带 storage = 测当前保存的设置”。
	if r := test(map[string]any{"type": "local"}); r.fields()["ok"] != true {
		t.Fatalf("local storage test = %s", r.body)
	}
	if r := e.admin("POST", "/storage/test", map[string]any{}).want(200); r.fields()["ok"] != true {
		t.Fatalf("storage test of the saved settings = %s", r.body)
	}

	// 不能用：message 要让人看得出是凭据不对、桶不存在还是地址不通。
	wantFailure(s3(map[string]any{"secret_access_key": "not-the-secret"}), "credentials")
	wantFailure(s3(map[string]any{"access_key_id": "nobody"}), "credentials")
	wantFailure(s3(map[string]any{"bucket": "no-such-bucket"}), "bucket")
	wantFailure(s3(map[string]any{"endpoint": "http://127.0.0.1:1"}), "cannot reach")
	wantFailure(s3(map[string]any{"endpoint": "http://no-such-host.invalid"}), "cannot reach")
	// 设置本身就不完整 / 不合法：指出是哪一项。
	wantFailure(map[string]any{"type": "s3"}, "storage.s3.endpoint")
	wantFailure(s3(map[string]any{"bucket": ""}), "storage.s3.bucket")
	wantFailure(s3(map[string]any{"secret_access_key": ""}), "storage.s3.secret_access_key")
	wantFailure(map[string]any{"type": "tape"}, "storage.type")

	// 什么都没保存，新存储里也没留下探测用的东西。
	if got := e.settingsOf(); describe(got) != describe(before) {
		t.Fatalf("testing changed the settings:\n%s", describe(got))
	}
	if keys := e.fake.Keys(t); len(keys) != 0 {
		t.Fatalf("probe objects left behind: %v", keys)
	}

	// secret_access_key 不带或为空 = 用已保存的那个。
	e.putSettings(map[string]any{"storage": s3(nil), "confirm_storage_change": true})
	without := s3(nil)
	delete(without["s3"].(map[string]any), "secret_access_key")
	if r := test(without); r.fields()["ok"] != true {
		t.Fatalf("test with the saved secret = %s", r.body)
	}
	if r := test(s3(map[string]any{"secret_access_key": ""})); r.fields()["ok"] != true {
		t.Fatalf("test with an empty secret = %s", r.body)
	}
	if r := test(map[string]any{"s3": map[string]any{"bucket": s3test.Bucket}}); r.fields()["ok"] != true {
		t.Fatalf("test with a partial object = %s", r.body)
	}
	wantFailure(s3(map[string]any{"secret_access_key": "not-the-secret"}), "credentials")

	e.admin("POST", "/storage/test", []byte("{nope")).wantError(400, "bad_request")
	e.admin("POST", "/storage/test", []byte("")).wantError(400, "bad_request")
}

// 启动对账：记录和存储里的对象对不上的各种情况。
func TestReconcileOnStart(t *testing.T) {
	e := newEnv(t, options{})
	dir := e.objectsDir()

	kept, keptID := bundle(t, 1200)
	e.mustUpload(keptID, kept, nil)
	lost, lostID := bundle(t, 900)
	e.mustUpload(lostID, lost, nil)
	// 有记录但对象没了。
	if err := os.Remove(filepath.Join(dir, lostID)); err != nil {
		t.Fatal(err)
	}
	// 有对象但没有记录，内容和名字对得上。
	orphan, orphanID := bundle(t, 700)
	os.WriteFile(filepath.Join(dir, orphanID), orphan, 0o644)
	// 有对象但没有记录，内容和名字对不上：没核对过的内容不能被下载到。
	bogus, _ := bundle(t, 600)
	bogusID := "0123456789abcdef0123456789abcdef"
	os.WriteFile(filepath.Join(dir, bogusID), bogus, 0o644)
	// 在禁止名单里却还留着对象。
	banned, bannedID := bundle(t, 500)
	e.admin("POST", "/blocklist", map[string]any{"id": bannedID}).want(200)
	os.WriteFile(filepath.Join(dir, bannedID), banned, 0o644)
	// 上次运行留下的临时文件。
	stale := filepath.Join(e.dataDir, "uploads", "ffffffffffffffffffffffffffffffff.part")
	os.WriteFile(stale, []byte("x"), 0o600)

	e = e.restart()

	e.wantDownloadable(keptID, kept)
	if got := e.status(lostID).str("state"); got != "missing" {
		t.Fatalf("lost: %q", got)
	}
	if st := e.status(orphanID); st.str("state") != "ready" || st.num("size") != 700 {
		t.Fatalf("orphan: %s", st.body)
	}
	e.wantDownloadable(orphanID, orphan)
	e.wantNotDownloadable(bogusID, "missing")
	if _, err := os.Stat(filepath.Join(dir, bogusID)); err != nil {
		t.Fatal("an object that failed verification must be left alone, not deleted")
	}
	e.wantNotDownloadable(bannedID, "blocked")
	if _, err := os.Stat(filepath.Join(dir, bannedID)); !os.IsNotExist(err) {
		t.Fatal("object of a blocked id was not deleted")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale temporary file was not removed")
	}

	list := e.admin("GET", "/models?sort=size&order=asc", nil).want(200)
	items := list.fields()["items"].([]any)
	if list.num("total") != 2 {
		t.Fatalf("list = %s", list.body)
	}
	first := items[0].(map[string]any)
	if first["id"] != orphanID || first["name"] != "(unknown)" || first["uploader"] != "" || first["uploaded_at"].(float64) <= 0 {
		t.Fatalf("orphan record = %s", describe(first))
	}

	// 对象在服务运行期间被人从存储里删掉：下载时发现，记录随之清掉，之后可以重新上传。
	if err := os.Remove(filepath.Join(dir, keptID)); err != nil {
		t.Fatal(err)
	}
	e.call("GET", "/api/v1/models/"+keptID+"/file", nil).wantError(404, "not_found")
	if got := e.status(keptID).str("state"); got != "missing" {
		t.Fatalf("state after the object vanished = %q", got)
	}
	e.mustUpload(keptID, kept, nil)
}

// 0.1.0 留下的数据目录（那时设置在配置文件里，数据里只有模型记录、禁止名单和 retention_hours）：
// 升级之后这些都原样可用，其余设置按首次启动处理。
func TestUpgradeFromOldDataDir(t *testing.T) {
	dataDir := t.TempDir()
	content, id := bundle(t, 1800)
	if err := os.MkdirAll(filepath.Join(dataDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "objects", id), content, 0o644); err != nil {
		t.Fatal(err)
	}
	blockedID := "ffffffffffffffffffffffffffffff01"

	// 照 0.1.0 的格式直接写数据文件。
	old, err := bolt.Open(filepath.Join(dataDir, "meta.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = old.Update(func(tx *bolt.Tx) error {
		put := func(bucket, key string, value any) error {
			b, err := tx.CreateBucketIfNotExists([]byte(bucket))
			if err != nil {
				return err
			}
			raw, ok := value.([]byte)
			if !ok {
				if raw, err = json.Marshal(value); err != nil {
					return err
				}
			}
			return b.Put([]byte(key), raw)
		}
		if err := put("models", id, map[string]any{
			"id": id, "name": "Old.vrca", "size": len(content), "uploader": "老玩家",
			"uploaded_at": 1700000000, "last_download_at": 1700003600, "download_count": 7,
		}); err != nil {
			return err
		}
		if err := put("blocklist", blockedID, map[string]any{"id": blockedID, "name": "Bad.vrca", "note": "old", "blocked_at": 1700000001}); err != nil {
			return err
		}
		return put("settings", "retention_hours", []byte("0"))
	})
	if err != nil {
		t.Fatal(err)
	}
	old.Close()

	e := newEnv(t, options{dataDir: dataDir})
	// 设置是头一回建：生成了密钥和初始密码；retention_hours 沿用旧值（0 = 不自动删除）。
	password := e.app.FirstRunPassword()
	if password == "" {
		t.Fatal("no initial password was generated for an old data directory")
	}
	set := e.app.Settings()
	if set.RetentionHours != 0 || set.AuthKey == "" || set.Storage.Type != "local" {
		t.Fatalf("settings = %+v", set)
	}

	// 模型记录和禁止名单原样可用。
	key := map[string]string{"Authorization": "Bearer " + set.AuthKey}
	if got := e.do("GET", "/api/v1/models/"+id+"/file", nil, key).want(200).body; !bytes.Equal(got, content) {
		t.Fatal("old model content differs")
	}
	if st := e.do("GET", "/api/v1/models/"+blockedID, nil, key).want(200); st.str("state") != "blocked" {
		t.Fatalf("old blocklist entry: %s", st.body)
	}
	cookie := adminHeaders(e.loginWith(password))
	list := e.do("GET", "/api/v1/admin/models", nil, cookie).want(200)
	item := list.fields()["items"].([]any)[0].(map[string]any)
	if list.num("total") != 1 || item["name"] != "Old.vrca" || item["uploader"] != "老玩家" || item["download_count"] != float64(7) ||
		item["uploaded_at"] != float64(1700000000) || item["expires_at"] != float64(0) {
		t.Fatalf("old model record = %s", describe(item))
	}
	blocked := e.do("GET", "/api/v1/admin/blocklist", nil, cookie).want(200).fields()["items"].([]any)
	if len(blocked) != 1 || blocked[0].(map[string]any)["note"] != "old" {
		t.Fatalf("old blocklist = %v", blocked)
	}
}
