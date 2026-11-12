package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"vml-sync-server/internal/app"
	"vml-sync-server/internal/s3test"
)

// settingsOf 取当前的设置（GET /settings 的整个对象）。
func (e *env) settingsOf() map[string]any {
	e.t.Helper()
	return e.admin("GET", "/settings", nil).want(200).fields()
}

// s3Settings 返回“把存储设成假 S3”的请求体片段。
func (e *env) s3Settings(accessKey, secretKey string) map[string]any {
	return map[string]any{"type": "s3", "s3": map[string]any{
		"endpoint": e.fake.URL, "region": s3test.Region, "bucket": s3test.Bucket,
		"access_key_id": accessKey, "secret_access_key": secretKey,
	}}
}

// 设置的形状和默认值（docs/API.md 3.5），以及局部更新只改带上的键。
func TestSettingsShapeAndPartialUpdate(t *testing.T) {
	e := newEnv(t, options{})

	want := map[string]any{
		"retention_hours": float64(72), "base_path": "", "auth_key": testAuthKey,
		"max_file_mb": float64(200), "max_total_mb": float64(0), "chunk_mb": float64(5), "max_active_uploads": float64(8),
		"transfer": "proxy", "direct_url_ttl_seconds": float64(900), "verify_direct_uploads": true,
		"trust_forwarded_headers": false,
		"storage": map[string]any{"type": "local", "s3": map[string]any{
			"endpoint": "", "region": "auto", "bucket": "", "access_key_id": "",
			"secret_access_key_set": false, "prefix": "models/", "path_style": true,
		}},
	}
	if got := e.settingsOf(); !reflect.DeepEqual(got, want) {
		t.Fatalf("default settings =\n%s\nwant\n%s", describe(got), describe(want))
	}

	// 只带一个键：只有它变，响应是改完之后的完整对象。
	r := e.putSettings(map[string]any{"max_file_mb": 50})
	want["max_file_mb"] = float64(50)
	if got := r.fields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PUT response =\n%s\nwant\n%s", describe(got), describe(want))
	}
	if got := e.settingsOf(); !reflect.DeepEqual(got, want) {
		t.Fatalf("settings after PUT =\n%s", describe(got))
	}

	// 一次带好几个键，包括嵌套对象里的一部分；不认识的键被忽略（页面可以把 GET 的对象原样改改再发回来）。
	r = e.putSettings(map[string]any{
		"retention_hours": 0, "max_active_uploads": 3, "verify_direct_uploads": false, "trust_forwarded_headers": true,
		"direct_url_ttl_seconds": 60, "max_total_mb": 1000, "chunk_mb": 8,
		"storage": map[string]any{"s3": map[string]any{
			"bucket": "later", "prefix": "x/", "path_style": false, "region": "", "secret_access_key_set": true,
		}},
		"something_new": 1,
	})
	want["retention_hours"], want["max_active_uploads"], want["verify_direct_uploads"] = float64(0), float64(3), false
	want["trust_forwarded_headers"], want["direct_url_ttl_seconds"] = true, float64(60)
	want["max_total_mb"], want["chunk_mb"] = float64(1000), float64(8)
	s3 := want["storage"].(map[string]any)["s3"].(map[string]any)
	// 空的 region 等于 "auto"；secret_access_key_set 是只读的，写它没有用。
	s3["bucket"], s3["prefix"], s3["path_style"] = "later", "x/", false
	if got := r.fields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PUT response =\n%s\nwant\n%s", describe(got), describe(want))
	}

	// 空对象：什么都不改。
	if got := e.putSettings(map[string]any{}).fields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("settings after an empty PUT =\n%s", describe(got))
	}

	// 概况和 ping 立刻反映新值。
	ov := e.admin("GET", "/overview", nil).want(200)
	if ov.num("retention_hours") != 0 || ov.num("max_file_bytes") != 50*unit || ov.num("max_total_bytes") != 1000*unit || ov.num("chunk_bytes") != 8*unit {
		t.Fatalf("overview = %s", ov.body)
	}
	ping := e.call("GET", "/api/v1/ping", nil).want(200)
	if ping.num("max_file_bytes") != 50*unit || ping.num("chunk_bytes") != 8*unit {
		t.Fatalf("ping = %s", ping.body)
	}

	// 重启之后都还在。
	e = e.restart()
	if got := e.settingsOf(); !reflect.DeepEqual(got, want) {
		t.Fatalf("settings after restart =\n%s\nwant\n%s", describe(got), describe(want))
	}
}

// 任何一项不合法：报出是哪一项，并且什么都不改。
func TestSettingsValidation(t *testing.T) {
	e := newEnv(t, options{})
	before := e.settingsOf()

	long := strings.Repeat("k", 129)
	cases := []struct {
		field string
		patch map[string]any
	}{
		{"retention_hours", map[string]any{"retention_hours": -1}},
		{"retention_hours", map[string]any{"retention_hours": 87601}},
		{"retention_hours", map[string]any{"retention_hours": 1.5}},
		{"retention_hours", map[string]any{"retention_hours": "24"}},
		{"base_path", map[string]any{"base_path": "no-slash"}},
		{"base_path", map[string]any{"base_path": "/ends-with/"}},
		{"base_path", map[string]any{"base_path": "/has space"}},
		{"base_path", map[string]any{"base_path": "/a/../b"}},
		{"base_path", map[string]any{"base_path": "/" + strings.Repeat("x", 64)}},
		{"base_path", map[string]any{"base_path": 5}},
		{"auth_key", map[string]any{"auth_key": "too-short"}},
		{"auth_key", map[string]any{"auth_key": long}},
		{"auth_key", map[string]any{"auth_key": "has a space inside it"}},
		{"auth_key", map[string]any{"auth_key": "密钥密钥密钥密钥密钥密钥密钥密钥"}},
		{"auth_key", map[string]any{"auth_key": ""}},
		{"max_file_mb", map[string]any{"max_file_mb": 0}},
		{"max_file_mb", map[string]any{"max_file_mb": 5001}},
		{"max_total_mb", map[string]any{"max_total_mb": -1}},
		{"chunk_mb", map[string]any{"chunk_mb": 0}},
		{"chunk_mb", map[string]any{"chunk_mb": 65}},
		{"max_active_uploads", map[string]any{"max_active_uploads": 0}},
		{"max_active_uploads", map[string]any{"max_active_uploads": 65}},
		{"transfer", map[string]any{"transfer": "fast"}},
		// direct 只有存储是 s3 时可用。
		{"transfer", map[string]any{"transfer": "direct"}},
		{"direct_url_ttl_seconds", map[string]any{"direct_url_ttl_seconds": 59}},
		{"direct_url_ttl_seconds", map[string]any{"direct_url_ttl_seconds": 86401}},
		{"verify_direct_uploads", map[string]any{"verify_direct_uploads": "yes"}},
		{"trust_forwarded_headers", map[string]any{"trust_forwarded_headers": 1}},
		{"storage.type", map[string]any{"storage": map[string]any{"type": "ftp"}}},
		{"storage", map[string]any{"storage": "s3"}},
		{"storage.s3.endpoint", map[string]any{"storage": map[string]any{"type": "s3"}}},
		{"storage.s3.endpoint", map[string]any{"storage": map[string]any{"s3": map[string]any{"endpoint": "not a url"}}}},
		{"storage.s3.endpoint", map[string]any{"storage": map[string]any{"s3": map[string]any{"endpoint": "https://host/bucket"}}}},
		{"storage.s3.bucket", map[string]any{"storage": map[string]any{"type": "s3", "s3": map[string]any{"endpoint": "https://host"}}}},
		{"storage.s3.bucket", map[string]any{"storage": map[string]any{"s3": map[string]any{"bucket": "a/b"}}}},
		{"storage.s3.access_key_id", map[string]any{"storage": map[string]any{"type": "s3", "s3": map[string]any{"endpoint": "https://host", "bucket": "b"}}}},
		{"storage.s3.secret_access_key", map[string]any{"storage": map[string]any{"type": "s3", "s3": map[string]any{
			"endpoint": "https://host", "bucket": "b", "access_key_id": "ak"}}}},
		{"storage.s3.prefix", map[string]any{"storage": map[string]any{"s3": map[string]any{"prefix": "/models/"}}}},
		{"storage.s3.path_style", map[string]any{"storage": map[string]any{"s3": map[string]any{"path_style": "yes"}}}},
		// 好几项一起改，其中一项不行：整个请求都不生效。
		{"chunk_mb", map[string]any{"max_file_mb": 10, "retention_hours": 1, "chunk_mb": 999, "auth_key": "another-valid-key-0123456789"}},
	}
	for _, tc := range cases {
		r := e.admin("PUT", "/settings", tc.patch).wantError(400, "invalid_settings")
		if r.str("field") != tc.field || r.str("message") == "" {
			t.Errorf("patch %v: field = %q, want %q (%s)", tc.patch, r.str("field"), tc.field, r.body)
		}
	}
	e.admin("PUT", "/settings", []byte("{not json")).wantError(400, "bad_request")
	e.admin("PUT", "/settings", []byte("")).wantError(400, "bad_request")
	e.admin("PUT", "/settings", []byte("[]")).wantError(400, "bad_request")

	if after := e.settingsOf(); !reflect.DeepEqual(after, before) {
		t.Fatalf("a rejected update changed the settings:\n%s\nwas\n%s", describe(after), describe(before))
	}
	e.call("GET", "/api/v1/ping", nil).want(200)

	// direct 还要求分片不小于 5 MB。
	s := newEnv(t, options{s3: true})
	s.admin("PUT", "/settings", map[string]any{"transfer": "direct", "chunk_mb": 4}).wantError(400, "invalid_settings")
	if r := s.admin("PUT", "/settings", map[string]any{"chunk_mb": 4, "transfer": "direct"}); r.str("field") != "chunk_mb" {
		t.Fatalf("direct with small chunks: %s", r.body)
	}
	s.putSettings(map[string]any{"transfer": "direct"})
	if r := s.admin("PUT", "/settings", map[string]any{"chunk_mb": 4}).wantError(400, "invalid_settings"); r.str("field") != "chunk_mb" {
		t.Fatalf("small chunks with direct: %s", r.body)
	}
	// 用着 direct 时不能把存储换回本地，除非同时把 transfer 改掉。
	if r := s.admin("PUT", "/settings", map[string]any{
		"storage": map[string]any{"type": "local"}, "confirm_storage_change": true,
	}).wantError(400, "invalid_settings"); r.str("field") != "transfer" {
		t.Fatalf("local storage with direct: %s", r.body)
	}
}

// 限额和分片大小热生效：对之后新开的上传生效，已经在传的会话按它开始时的值走完。
func TestLimitsApplyToNewUploads(t *testing.T) {
	e := newEnv(t, options{})
	old, oldID := bundle(t, 2500)
	// 用 1000 字节一片开始的会话，先传一片。
	inFlight := e.begin(oldID, len(old), nil).wantState("upload")
	if inFlight.num("chunk_bytes") != 1000 || inFlight.num("part_count") != 3 {
		t.Fatalf("begin = %s", inFlight.body)
	}
	oldUpload := inFlight.str("upload_id")
	e.call("PUT", "/api/v1/uploads/"+oldUpload+"/parts/1", slice(old, 1, 1000)).want(200)

	// 把分片改成 400 字节、单个文件上限改成 2000 字节、同时进行的上传上限改成 2。
	e.putSettings(map[string]any{"chunk_mb": 2, "max_file_mb": 10, "max_active_uploads": 2})
	if ping := e.call("GET", "/api/v1/ping", nil).want(200); ping.num("chunk_bytes") != 400 || ping.num("max_file_bytes") != 2000 {
		t.Fatalf("ping = %s", ping.body)
	}

	// 新的上传用新的值。
	big, bigID := bundle(t, 2001)
	if r := e.begin(bigID, len(big), nil).wantState("too_large"); r.num("max_file_bytes") != 2000 {
		t.Fatalf("too_large = %s", r.body)
	}
	fresh, freshID := bundle(t, 1000)
	r := e.begin(freshID, len(fresh), nil).wantState("upload")
	if r.num("chunk_bytes") != 400 || r.num("part_count") != 3 {
		t.Fatalf("begin after the change = %s", r.body)
	}
	// 现在有两个会话了，到了新的上限。
	third, thirdID := bundle(t, 500)
	e.begin(thirdID, len(third), nil).wantState("busy")

	// 已经在传的会话不受影响：还是 1000 字节一片（比现在的分片大也照收），而且它比现在的文件上限大也能传完。
	e.call("PUT", "/api/v1/uploads/"+oldUpload+"/parts/2", slice(old, 2, 1000)).want(200)
	e.call("PUT", "/api/v1/uploads/"+oldUpload+"/parts/3", slice(old, 3, 1000)).want(200)
	e.complete(oldUpload).wantState("ready")
	e.wantDownloadable(oldID, old)

	// 新会话按 400 字节一片传；按旧的大小发会被拒绝。
	newUpload := r.str("upload_id")
	e.call("PUT", "/api/v1/uploads/"+newUpload+"/parts/1", fresh[:1000]).wantError(413, "too_large")
	for n := 1; n <= 3; n++ {
		e.call("PUT", "/api/v1/uploads/"+newUpload+"/parts/"+string(rune('0'+n)), slice(fresh, n, 400)).want(200)
	}
	e.complete(newUpload).wantState("ready")
	e.wantDownloadable(freshID, fresh)

	// 总容量上限：改小之后新的上传放不下。
	e.putSettings(map[string]any{"max_total_mb": 18}) // 3600 字节，已经用了 3500
	e.begin(thirdID, len(third), nil).wantState("storage_full")
	e.putSettings(map[string]any{"max_total_mb": 0})
	e.mustUpload(thirdID, third, nil)
}

// 传输方式热切换：对之后的上传和下载生效。
func TestTransferSwitch(t *testing.T) {
	e := newEnv(t, options{s3: true})
	a, aID := bundle(t, 2500)
	e.mustUpload(aID, a, nil)
	if dl := e.call("POST", "/api/v1/models/"+aID+"/download", nil).wantState("ready"); dl.str("mode") != "proxy" {
		t.Fatalf("download = %s", dl.body)
	}

	// 一个中转的会话传到一半时改成 direct。
	b, bID := bundle(t, 2500)
	proxyUpload := e.upload(bID, b, nil, 3)
	e.putSettings(map[string]any{"transfer": "direct"})
	if got := e.call("GET", "/api/v1/ping", nil).want(200).str("transfer"); got != "direct" {
		t.Fatalf("transfer = %q", got)
	}

	// 已经在传的会话还是中转，照常传完。
	e.putPart(proxyUpload, b, 3).want(200)
	e.complete(proxyUpload).wantState("ready")

	// 之后的上传和下载都走直连。
	dl := e.call("POST", "/api/v1/models/"+aID+"/download", nil).wantState("ready")
	if dl.str("mode") != "direct" || dl.str("url") == "" {
		t.Fatalf("download = %s", dl.body)
	}
	if got := e.do("GET", dl.str("url"), nil, nil).want(200).body; !bytes.Equal(got, a) {
		t.Fatal("content from the direct url differs")
	}
	c, cID := bundle(t, 2500)
	if r := e.begin(cID, len(c), nil).wantState("upload"); r.str("mode") != "direct" {
		t.Fatalf("begin = %s", r.body)
	} else {
		e.call("POST", "/api/v1/uploads/"+r.str("upload_id")+"/abort", nil).want(200)
	}
	e.mustUpload(cID, c, nil)

	// 临时地址的有效期也跟着设置走。
	e.putSettings(map[string]any{"direct_url_ttl_seconds": 120})
	if dl := e.call("POST", "/api/v1/models/"+cID+"/download", nil).wantState("ready"); dl.num("expires_in") != 120 ||
		!strings.Contains(dl.str("url"), "X-Amz-Expires=120") {
		t.Fatalf("download = %s", dl.body)
	}

	e.putSettings(map[string]any{"transfer": "proxy"})
	if dl := e.call("POST", "/api/v1/models/"+cID+"/download", nil).wantState("ready"); dl.str("mode") != "proxy" {
		t.Fatalf("download = %s", dl.body)
	}
}

// 改 auth_key：旧的立刻失效。
func TestAuthKeyChange(t *testing.T) {
	e := newEnv(t, options{})
	content, id := bundle(t, 1500)
	e.mustUpload(id, content, nil)

	const newKey = "a-brand-new-client-key-98765"
	if got := e.putSettings(map[string]any{"auth_key": newKey}).str("auth_key"); got != newKey {
		t.Fatalf("auth_key = %q", got)
	}
	e.call("GET", "/api/v1/ping", nil).wantError(401, "unauthorized")
	e.call("GET", "/api/v1/models/"+id+"/file", nil).wantError(401, "unauthorized")
	withNew := map[string]string{"Authorization": "Bearer " + newKey}
	e.do("GET", "/api/v1/ping", nil, withNew).want(200)
	if got := e.do("GET", "/api/v1/models/"+id+"/file", nil, withNew).want(200).body; !bytes.Equal(got, content) {
		t.Fatal("content differs")
	}
	// 重启之后还是新的那个。
	e = e.restart()
	e.call("GET", "/api/v1/ping", nil).wantError(401, "unauthorized")
	e.do("GET", "/api/v1/ping", nil, withNew).want(200)
}

// 改 base_path：响应照常返回并带着新路径下的 Cookie；从下一个请求起只认新前缀。
func TestBasePathChange(t *testing.T) {
	e := newEnv(t, options{})
	e.login()
	token := e.adminCookie.Value
	origin := e.srv.URL
	client := map[string]string{"Authorization": "Bearer " + testAuthKey}

	r := e.admin("PUT", "/settings", map[string]any{"base_path": "/vml-3f9a1c"}).want(200)
	if r.str("base_path") != "/vml-3f9a1c" {
		t.Fatalf("PUT response = %s", r.body)
	}
	// 同一个响应里：旧路径下的 Cookie 作废，新路径下发一个同样的会话。
	cookies := (&http.Response{Header: r.header}).Cookies()
	var expired, moved *http.Cookie
	for _, c := range cookies {
		switch c.Path {
		case "/":
			expired = c
		case "/vml-3f9a1c/":
			moved = c
		}
	}
	if len(cookies) != 2 || expired == nil || moved == nil {
		t.Fatalf("Set-Cookie = %q", r.header.Values("Set-Cookie"))
	}
	if expired.MaxAge >= 0 || expired.Value != "" {
		t.Fatalf("cookie under the old path was not expired: %q", r.header.Values("Set-Cookie"))
	}
	if moved.Value != token || moved.MaxAge <= 0 || moved.MaxAge > 43200 || !moved.HttpOnly || moved.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie under the new path: %q", r.header.Values("Set-Cookie"))
	}

	// 旧前缀：和 base_path 之外的任何地址一样，什么都没有。
	for _, path := range []string{"/api/v1/ping", "/admin/", "/api/v1/admin/session", "/api/v1/admin/settings"} {
		old := e.do("GET", origin+path, nil, map[string]string{"Authorization": client["Authorization"], "Cookie": "vml_admin=" + token})
		if old.want(404); strings.Contains(string(old.body), "{") {
			t.Fatalf("old prefix %s still answers: %s", path, old.body)
		}
	}
	// 新前缀：客户端接口可用，响应里的新 Cookie 可以直接用。
	e.do("GET", origin+"/vml-3f9a1c/api/v1/ping", nil, client).want(200)
	e.do("GET", origin+"/vml-3f9a1c/admin/", nil, nil).want(200)
	withMoved := adminHeaders(moved)
	if s := e.do("GET", origin+"/vml-3f9a1c/api/v1/admin/session", nil, withMoved).want(200); s.fields()["authenticated"] != true {
		t.Fatalf("session with the moved cookie = %s", s.body)
	}
	e.do("GET", origin+"/vml-3f9a1c/api/v1/admin/overview", nil, withMoved).want(200)
	// 之后的登录 Cookie 也在新路径下。
	login := e.do("POST", origin+"/vml-3f9a1c/api/v1/admin/login", map[string]any{"password": testAdminPassword}, nil).want(200)
	if c := login.header.Get("Set-Cookie"); !strings.Contains(c, "Path=/vml-3f9a1c/;") {
		t.Fatalf("Set-Cookie after the change = %q", c)
	}

	// 再改一次，这次从一个前缀换到另一个；浏览器这时可能同时带着两个同名 Cookie。
	r = e.do("PUT", origin+"/vml-3f9a1c/api/v1/admin/settings", map[string]any{"base_path": "/other/place"}, map[string]string{
		"Cookie": "vml_admin=stale-value-from-another-path; vml_admin=" + token, "X-VML-Admin": "1",
	}).want(200)
	set := r.header.Values("Set-Cookie")
	if len(set) != 2 || !strings.Contains(set[0]+set[1], "Path=/vml-3f9a1c/; Max-Age=0") || !strings.Contains(set[0]+set[1], "Path=/other/place/; Max-Age=") {
		t.Fatalf("Set-Cookie = %q", set)
	}
	e.do("GET", origin+"/vml-3f9a1c/api/v1/ping", nil, client).want(404)
	e.do("GET", origin+"/other/place/api/v1/ping", nil, client).want(200)

	// 重启之后入口还在新的地方；改回空前缀也行。
	e = e.restart()
	e.do("GET", origin2(e)+"/other/place/api/v1/ping", nil, client).want(200)
	e.do("GET", origin2(e)+"/api/v1/ping", nil, client).want(404)
	e.login()
	e.putSettings(map[string]any{"base_path": ""})
	e.do("GET", origin2(e)+"/api/v1/ping", nil, client).want(200)
	e.do("GET", origin2(e)+"/other/place/api/v1/ping", nil, client).wantError(404, "not_found")
}

func origin2(e *env) string { return e.srv.URL }

// 首次启动：生成客户端密钥和管理员初始密码；之后的启动不再生成。
func TestFirstStart(t *testing.T) {
	e := newEnv(t, options{fresh: true})

	password := e.app.FirstRunPassword()
	if len(password) < 16 || strings.ContainsAny(password, "0O1lIio") {
		t.Fatalf("initial password %q is too short or uses look-alike characters", password)
	}
	file := filepath.Join(e.dataDir, "initial-admin-password.txt")
	raw, err := os.ReadFile(file)
	if err != nil || strings.TrimSpace(string(raw)) != password {
		t.Fatalf("initial password file: %q, %v", raw, err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(file); info.Mode().Perm() != 0o600 {
			t.Fatalf("initial password file mode = %v, want 0600", info.Mode().Perm())
		}
	}

	set := e.app.Settings()
	if n := len(set.AuthKey); n < 16 || n > 128 || strings.ContainsAny(set.AuthKey, " \t") {
		t.Fatalf("generated auth_key %q", set.AuthKey)
	}
	// 其余设置是默认值，服务直接可用。
	if set.RetentionHours != 72 || set.BasePath != "" || set.MaxFileMB != 200 || set.ChunkMB != 5 || set.Transfer != "proxy" ||
		set.Storage.Type != "local" || !set.VerifyDirectUploads || set.TrustForwardedHeaders {
		t.Fatalf("default settings = %+v", set)
	}
	key := map[string]string{"Authorization": "Bearer " + set.AuthKey}
	e.do("GET", "/api/v1/ping", nil, key).want(200)
	e.call("GET", "/api/v1/ping", nil).wantError(401, "unauthorized")

	// 用初始密码登录；概况提醒还在用初始密码；设置里能看到客户端密钥。
	e.do("POST", "/api/v1/admin/login", map[string]any{"password": testAdminPassword}, nil).wantError(401, "bad_password")
	cookie := e.loginWith(password)
	ov := e.do("GET", "/api/v1/admin/overview", nil, adminHeaders(cookie)).want(200)
	if ov.fields()["initial_password"] != true {
		t.Fatalf("overview = %s", ov.body)
	}
	if got := e.do("GET", "/api/v1/admin/settings", nil, adminHeaders(cookie)).want(200).str("auth_key"); got != set.AuthKey {
		t.Fatalf("auth_key in settings = %q", got)
	}

	// 第二次启动：不再生成，密钥和密码都还是原来的，文件原样留着。
	e = e.restart()
	if e.app.FirstRunPassword() != "" {
		t.Fatal("a second start generated a new initial password")
	}
	if e.app.Settings().AuthKey != set.AuthKey {
		t.Fatal("auth_key changed across a restart")
	}
	if again, _ := os.ReadFile(file); string(again) != string(raw) {
		t.Fatal("initial password file changed across a restart")
	}
	if !e.app.InitialPassword() {
		t.Fatal("InitialPassword should still be true")
	}
	e.loginWith(password)
}

// 修改管理员密码（docs/API.md 3.7）。
func TestPasswordChange(t *testing.T) {
	e := newEnv(t, options{fresh: true})
	initial := e.app.FirstRunPassword()
	file := filepath.Join(e.dataDir, "initial-admin-password.txt")

	mine := adminHeaders(e.loginWith(initial))
	other := adminHeaders(e.loginWith(initial))
	change := func(current, next string) response {
		return e.do("POST", "/api/v1/admin/password", map[string]any{"current": current, "new": next}, mine)
	}
	const newPassword = "correct horse 电池 staple"

	// 新密码不合要求：太短、太长。当前密码是对的，所以不计入限速。
	for _, weak := range []string{"", "short", "1234567", strings.Repeat("长", 129)} {
		if r := change(initial, weak).wantError(400, "weak_password"); r.str("message") == "" {
			t.Fatalf("weak_password without a message: %s", r.body)
		}
	}
	// 当前密码不对：401，并且和登录失败一样计入限速。
	for i := 0; i < 5; i++ {
		change("wrong-password", newPassword).wantError(401, "bad_password")
	}
	if r := change(initial, newPassword).wantError(429, "too_many_attempts"); r.num("retry_after") <= 0 {
		t.Fatalf("429 = %s", r.body)
	}
	e.do("POST", "/api/v1/admin/login", map[string]any{"password": initial}, nil).wantError(429, "too_many_attempts")
	// 这些失败的尝试什么都没改。
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("initial password file disappeared: %v", err)
	}
	if ov := e.do("GET", "/api/v1/admin/overview", nil, mine).want(200); ov.fields()["initial_password"] != true {
		t.Fatalf("overview = %s", ov.body)
	}
	e.clock.Advance(5*time.Minute + time.Second)

	// 不带 X-VML-Admin 不行。
	e.do("POST", "/api/v1/admin/password", map[string]any{"current": initial, "new": newPassword},
		map[string]string{"Cookie": mine["Cookie"]}).wantError(400, "bad_request")

	// 改成功：8 个字符就够，128 个字符也行。
	if r := change(initial, "8 chars!").want(200); r.fields()["ok"] != true {
		t.Fatalf("change = %s", r.body)
	}
	change("8 chars!", strings.Repeat("长", 128)).want(200)
	change(strings.Repeat("长", 128), newPassword).want(200)

	// 别的会话作废，发起请求的这个保留。
	e.do("GET", "/api/v1/admin/overview", nil, other).wantError(401, "unauthorized")
	ov := e.do("GET", "/api/v1/admin/overview", nil, mine).want(200)
	// 不再是初始密码，初始密码文件也删掉了。
	if ov.fields()["initial_password"] != false {
		t.Fatalf("overview = %s", ov.body)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("initial password file still exists (%v)", err)
	}
	// 旧密码不能用了，新密码可以。
	e.do("POST", "/api/v1/admin/login", map[string]any{"password": initial}, nil).wantError(401, "bad_password")
	e.loginWith(newPassword)

	// 重启之后也一样，而且不会再生成初始密码。
	e = e.restart()
	if e.app.FirstRunPassword() != "" || e.app.InitialPassword() {
		t.Fatal("restart brought the initial password back")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("restart recreated the initial password file")
	}
	e.do("POST", "/api/v1/admin/login", map[string]any{"password": initial}, nil).wantError(401, "bad_password")
	e.loginWith(newPassword)
}

// -reset-admin-password：生成新的管理员密码，写进初始密码文件。
func TestResetAdminPassword(t *testing.T) {
	e := newEnv(t, options{})
	content, id := bundle(t, 1500)
	e.mustUpload(id, content, nil)
	e.putSettings(map[string]any{"retention_hours": 5})

	// 服务还开着的时候数据文件被占着，重置不了——所以要先停掉。
	e.srv.Close()
	e.app.Close()
	e.db.Close()
	password := resetAdminPassword(t, e.dataDir)
	if len(password) < 16 {
		t.Fatalf("new password %q is too short", password)
	}
	file := filepath.Join(e.dataDir, "initial-admin-password.txt")
	if raw, err := os.ReadFile(file); err != nil || strings.TrimSpace(string(raw)) != password {
		t.Fatalf("initial password file: %q, %v", raw, err)
	}

	// 再启动：只有密码变了，别的都还在。
	o := e.opts
	o.dataDir, o.clock = e.dataDir, e.clock
	e = newEnv(t, o)
	if e.app.FirstRunPassword() != "" {
		t.Fatal("start after a reset generated yet another password")
	}
	e.do("POST", "/api/v1/admin/login", map[string]any{"password": testAdminPassword}, nil).wantError(401, "bad_password")
	cookie := e.loginWith(password)
	ov := e.do("GET", "/api/v1/admin/overview", nil, adminHeaders(cookie)).want(200)
	if ov.fields()["initial_password"] != true || ov.num("retention_hours") != 5 || ov.num("model_count") != 1 {
		t.Fatalf("overview = %s", ov.body)
	}
	e.wantDownloadable(id, content)
}

// resetAdminPassword 做 -reset-admin-password 做的事：打开数据文件、换密码、关掉。
func resetAdminPassword(t *testing.T, dataDir string) string {
	t.Helper()
	db := openMeta(t, dataDir)
	defer db.Close()
	password, err := app.ResetAdminPassword(db, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return password
}

// GET /settings 的响应体里任何地方都不能出现 secret_access_key。
func assertNoSecret(t *testing.T, r response, secrets ...string) {
	t.Helper()
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if k == "secret_access_key" {
					t.Fatalf("response exposes secret_access_key: %s", r.body)
				}
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	var decoded any
	if err := json.Unmarshal(r.body, &decoded); err != nil {
		t.Fatal(err)
	}
	walk(decoded)
	for _, s := range secrets {
		if bytes.Contains(r.body, []byte(s)) {
			t.Fatalf("response contains a secret: %s", r.body)
		}
	}
}
