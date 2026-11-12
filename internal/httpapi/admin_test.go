package httpapi_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vml-sync-server/internal/version"
)

// 管理接口的鉴权：登录会话 Cookie、X-VML-Admin 请求头、会话过期。
func TestAdminAuth(t *testing.T) {
	e := newEnv(t, options{})

	// 没登录：其余接口一律 401；session 接口永远是 200。
	for _, req := range [][2]string{
		{"GET", "/overview"}, {"GET", "/models"}, {"POST", "/models/delete"}, {"GET", "/blocklist"},
		{"POST", "/blocklist"}, {"POST", "/blocklist/remove"}, {"GET", "/settings"}, {"PUT", "/settings"},
		{"POST", "/cleanup"}, {"GET", "/uploads"}, {"POST", "/password"}, {"POST", "/storage/test"},
		{"GET", "/does-not-exist"},
	} {
		headers := map[string]string{"X-VML-Admin": "1", "Cookie": "vml_admin=" + strings.Repeat("0", 64)}
		e.do(req[0], "/api/v1/admin"+req[1], nil, headers).wantError(401, "unauthorized")
		// 客户端密钥不能当管理员凭据用。
		headers = map[string]string{"X-VML-Admin": "1", "Authorization": "Bearer " + testAuthKey}
		e.do(req[0], "/api/v1/admin"+req[1], nil, headers).wantError(401, "unauthorized")
	}
	if s := e.do("GET", "/api/v1/admin/session", nil, nil).want(200); s.fields()["authenticated"] != false {
		t.Fatalf("session = %s", s.body)
	}

	// 登录。
	e.do("POST", "/api/v1/admin/login", map[string]any{"password": "nope"}, nil).wantError(401, "bad_password")
	e.do("POST", "/api/v1/admin/login", map[string]any{"password": testAuthKey}, nil).wantError(401, "bad_password")
	e.do("POST", "/api/v1/admin/login", []byte("{"), nil).wantError(400, "bad_request")
	login := e.do("POST", "/api/v1/admin/login", map[string]any{"password": testAdminPassword}, nil).want(200)
	if login.fields()["ok"] != true {
		t.Fatalf("login = %s", login.body)
	}
	cookie := login.header.Get("Set-Cookie")
	for _, want := range []string{"vml_admin=", "HttpOnly", "SameSite=Strict", "Path=/;", "Max-Age=43200"} {
		if !strings.Contains(cookie, want) {
			t.Fatalf("Set-Cookie %q is missing %q", cookie, want)
		}
	}
	if strings.Contains(cookie, "Secure") {
		t.Fatalf("Set-Cookie over plain HTTP must not be Secure: %q", cookie)
	}
	token := strings.TrimPrefix(strings.SplitN(cookie, ";", 2)[0], "vml_admin=")
	if len(token) < 32 {
		t.Fatalf("session token too short: %q", token)
	}
	session := map[string]string{"Cookie": "vml_admin=" + token}
	withHeader := map[string]string{"Cookie": "vml_admin=" + token, "X-VML-Admin": "1"}

	if s := e.do("GET", "/api/v1/admin/session", nil, session).want(200); s.fields()["authenticated"] != true {
		t.Fatalf("session = %s", s.body)
	}
	e.do("GET", "/api/v1/admin/overview", nil, session).want(200)
	e.do("GET", "/api/v1/admin/does-not-exist", nil, session).wantError(404, "not_found")

	// 会改动数据的请求必须带 X-VML-Admin: 1。
	for _, req := range [][2]string{
		{"POST", "/models/delete"}, {"POST", "/blocklist"}, {"POST", "/blocklist/remove"},
		{"PUT", "/settings"}, {"POST", "/cleanup"}, {"POST", "/logout"},
		{"POST", "/password"}, {"POST", "/storage/test"},
	} {
		e.do(req[0], "/api/v1/admin"+req[1], map[string]any{"retention_hours": 1, "ids": []string{}}, session).wantError(400, "bad_request")
		wrong := map[string]string{"Cookie": "vml_admin=" + token, "X-VML-Admin": "0"}
		e.do(req[0], "/api/v1/admin"+req[1], map[string]any{"retention_hours": 1, "ids": []string{}}, wrong).wantError(400, "bad_request")
	}
	// 没带请求头的那些请求什么都没改。
	if got := e.do("GET", "/api/v1/admin/settings", nil, session).want(200).num("retention_hours"); got != 72 {
		t.Fatalf("retention_hours = %d", got)
	}
	e.do("POST", "/api/v1/admin/cleanup", nil, withHeader).want(200)

	// 会话 12 小时过期。
	e.clock.Advance(12*time.Hour - time.Second)
	e.do("GET", "/api/v1/admin/overview", nil, session).want(200)
	e.clock.Advance(2 * time.Second)
	e.do("GET", "/api/v1/admin/overview", nil, session).wantError(401, "unauthorized")
	if s := e.do("GET", "/api/v1/admin/session", nil, session).want(200); s.fields()["authenticated"] != false {
		t.Fatalf("session after expiry = %s", s.body)
	}

	// 登出让会话作废。
	e.adminCookie = nil
	e.admin("GET", "/overview", nil).want(200)
	out := e.admin("POST", "/logout", nil).want(200)
	if out.fields()["ok"] != true || !strings.Contains(out.header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout = %s %q", out.body, out.header.Get("Set-Cookie"))
	}
	e.admin("GET", "/overview", nil).wantError(401, "unauthorized")
}

// 登录失败限速：同一来源 5 分钟内失败满 5 次，之后一律 429。
func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, options{})
	login := func(password string) response {
		return e.do("POST", "/api/v1/admin/login", map[string]any{"password": password}, nil)
	}

	for i := 0; i < 4; i++ {
		login("wrong").wantError(401, "bad_password")
		e.clock.Advance(10 * time.Second)
	}
	// 还没满 5 次：密码对了就能进，并且清零。
	login(testAdminPassword).want(200)

	for i := 0; i < 5; i++ {
		login("wrong").wantError(401, "bad_password")
		e.clock.Advance(10 * time.Second)
	}
	// 满了：连正确的密码也不行。
	r := login(testAdminPassword).wantError(429, "too_many_attempts")
	// 最早的一次失败是 50 秒前，还要等 250 秒。
	if r.num("retry_after") != 250 || r.header.Get("Retry-After") != "250" {
		t.Fatalf("429 = %s, Retry-After %q", r.body, r.header.Get("Retry-After"))
	}
	login("wrong").wantError(429, "too_many_attempts")

	// 被挡住的尝试不会让等待时间越拖越长。
	e.clock.Advance(249 * time.Second)
	login(testAdminPassword).wantError(429, "too_many_attempts")
	e.clock.Advance(2 * time.Second)
	login(testAdminPassword).want(200)
}

// 在受信任的反向代理后面：来源地址和是否 HTTPS 取自转发头。
func TestForwardedHeaders(t *testing.T) {
	e := newEnv(t, options{trustForwarded: true})
	login := func(password, forwardedFor string) response {
		return e.do("POST", "/api/v1/admin/login", map[string]any{"password": password}, map[string]string{
			"X-Forwarded-For":   forwardedFor,
			"X-Forwarded-Proto": "https",
		})
	}
	// 限速按代理看到的地址（最后一项）算；前面那些是客户端自己能伪造的，换来换去没用。
	for i := 0; i < 5; i++ {
		login("wrong", fmt.Sprintf("10.0.0.%d, 203.0.113.7", i)).wantError(401, "bad_password")
	}
	login(testAdminPassword, "10.9.9.9, 203.0.113.7").wantError(429, "too_many_attempts")
	// 别的来源不受影响，而且 Cookie 带 Secure。
	ok := login(testAdminPassword, "203.0.113.8").want(200)
	if !strings.Contains(ok.header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("Set-Cookie = %q", ok.header.Get("Set-Cookie"))
	}

	// 没打开信任时，转发头不起作用。
	plain := newEnv(t, options{})
	r := plain.do("POST", "/api/v1/admin/login", map[string]any{"password": testAdminPassword}, map[string]string{
		"X-Forwarded-Proto": "https",
	}).want(200)
	if strings.Contains(r.header.Get("Set-Cookie"), "Secure") {
		t.Fatalf("Set-Cookie = %q", r.header.Get("Set-Cookie"))
	}
}

// 概况。
func TestAdminOverview(t *testing.T) {
	e := newEnv(t, options{maxTotal: 100_000})
	a, aID := bundle(t, 1200)
	e.mustUpload(aID, a, nil)
	b, bID := bundle(t, 800)
	e.mustUpload(bID, b, nil)
	c, cID := bundle(t, 2500)
	e.upload(cID, c, nil, 3) // 传到一半
	_, dID := bundle(t, 10)
	e.admin("POST", "/blocklist", map[string]any{"id": dID}).want(200)
	cleaned := e.admin("POST", "/cleanup", nil).want(200)
	if cleaned.num("deleted") != 0 || cleaned.num("freed_bytes") != 0 {
		t.Fatalf("cleanup = %s", cleaned.body)
	}

	ov := e.admin("GET", "/overview", nil).want(200)
	now := e.clock.Now().Unix()
	want := map[string]any{
		"version": version.Version, "storage": "local", "transfer": "proxy",
		"max_file_bytes": float64(200 * unit), "max_total_bytes": float64(100_000), "chunk_bytes": float64(1000),
		"model_count": float64(2), "total_bytes": float64(2000), "blocked_count": float64(1), "active_uploads": float64(1),
		"retention_hours": float64(72), "last_cleanup_at": float64(now), "now": float64(now),
		"initial_password": false,
	}
	got := ov.fields()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("overview %s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["next_cleanup_at"].(float64); !ok || len(got) != len(want)+2 {
		t.Fatalf("overview = %s", ov.body)
	}
	// 数据目录所在磁盘的剩余空间：在跑测试的这些系统上都拿得到。
	if free, ok := got["disk_free_bytes"].(float64); !ok || free <= 0 {
		t.Fatalf("disk_free_bytes = %v", got["disk_free_bytes"])
	}

	uploads := e.admin("GET", "/uploads", nil).want(200).fields()["items"].([]any)
	if len(uploads) != 1 {
		t.Fatalf("uploads = %v", uploads)
	}
	u := uploads[0].(map[string]any)
	if u["id"] != cID || u["name"] != "model.vrca" || u["uploader"] != "tester" || u["size"] != float64(2500) ||
		u["received"] != float64(2000) || u["mode"] != "proxy" || u["started_at"] != float64(now) || len(u) != 7 {
		t.Fatalf("upload item = %s", describe(u))
	}
}

// 模型列表：搜索、排序、分页。
func TestAdminModelList(t *testing.T) {
	e := newEnv(t, options{})
	type model struct {
		name, uploader string
		size           int
		id             string
	}
	models := []*model{
		{name: "Chiffon.vrca", uploader: "Alice", size: 3000},
		{name: "kikyo_v2.vrca", uploader: "bob", size: 1000},
		{name: "Manuka.vrca", uploader: "alice", size: 2000},
		{name: "桔梗.vrca", uploader: "小明", size: 1500},
	}
	for _, m := range models {
		content, id := bundle(t, m.size)
		m.id = id
		e.mustUpload(id, content, map[string]any{"name": m.name, "uploader": m.uploader})
		e.clock.Advance(time.Hour)
	}
	// kikyo 被下载两次，Manuka 一次。
	e.call("POST", "/api/v1/models/"+models[1].id+"/download", nil).wantState("ready")
	e.clock.Advance(time.Minute)
	e.call("POST", "/api/v1/models/"+models[1].id+"/download", nil).wantState("ready")
	e.clock.Advance(time.Minute)
	e.call("POST", "/api/v1/models/"+models[2].id+"/download", nil).wantState("ready")

	names := func(query string) (int64, []string) {
		t.Helper()
		r := e.admin("GET", "/models"+query, nil).want(200)
		var out []string
		for _, it := range r.fields()["items"].([]any) {
			out = append(out, it.(map[string]any)["name"].(string))
		}
		return r.num("total"), out
	}
	check := func(query string, wantTotal int64, want ...string) {
		t.Helper()
		total, got := names(query)
		if total != wantTotal || strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("list %q = total %d, %v; want total %d, %v", query, total, got, wantTotal, want)
		}
	}

	// 默认：按上传时间从新到旧。
	check("", 4, "桔梗.vrca", "Manuka.vrca", "kikyo_v2.vrca", "Chiffon.vrca")
	check("?sort=uploaded_at&order=asc", 4, "Chiffon.vrca", "kikyo_v2.vrca", "Manuka.vrca", "桔梗.vrca")
	check("?sort=size&order=asc", 4, "kikyo_v2.vrca", "桔梗.vrca", "Manuka.vrca", "Chiffon.vrca")
	check("?sort=size&order=desc", 4, "Chiffon.vrca", "Manuka.vrca", "桔梗.vrca", "kikyo_v2.vrca")
	check("?sort=name&order=asc", 4, "Chiffon.vrca", "kikyo_v2.vrca", "Manuka.vrca", "桔梗.vrca")
	check("?sort=download_count&order=desc&limit=2", 4, "kikyo_v2.vrca", "Manuka.vrca")
	check("?sort=last_download_at&order=desc&limit=2", 4, "Manuka.vrca", "kikyo_v2.vrca")
	// 过期时间 = 最后使用时间 + 保留时长：被下载过的排在最后过期。
	check("?sort=expires_at&order=asc", 4, "Chiffon.vrca", "桔梗.vrca", "kikyo_v2.vrca", "Manuka.vrca")
	total, byUploader := names("?sort=uploader&order=asc")
	if total != 4 || byUploader[2] != "kikyo_v2.vrca" || byUploader[3] != "桔梗.vrca" {
		t.Fatalf("sort by uploader = %v", byUploader)
	}

	// 分页。
	check("?sort=size&order=asc&offset=1&limit=2", 4, "桔梗.vrca", "Manuka.vrca")
	check("?sort=size&order=asc&offset=3&limit=50", 4, "Chiffon.vrca")
	check("?offset=10", 4)
	check("?limit=100000", 4, "桔梗.vrca", "Manuka.vrca", "kikyo_v2.vrca", "Chiffon.vrca")

	// 搜索：文件名、上传者、id，不区分大小写。
	check("?q=CHIFFON", 1, "Chiffon.vrca")
	check("?q=alice&sort=size&order=asc", 2, "Manuka.vrca", "Chiffon.vrca")
	check("?q=%E6%A1%94%E6%A2%97", 1, "桔梗.vrca")
	check("?q=%E5%B0%8F%E6%98%8E", 1, "桔梗.vrca")
	check("?q="+models[1].id[4:20], 1, "kikyo_v2.vrca")
	check("?q="+strings.ToUpper(models[1].id[4:20]), 1, "kikyo_v2.vrca")
	check("?q=no-such-thing", 0)
	check("?q=.vrca&sort=size&order=asc&limit=1&offset=1", 4, "桔梗.vrca")

	// 空列表是 []，不是 null。
	if r := e.admin("GET", "/models?q=no-such-thing", nil); !bytes.Contains(r.body, []byte(`"items":[]`)) {
		t.Fatalf("empty list = %s", r.body)
	}

	for _, bad := range []string{"?sort=nope", "?order=up", "?offset=x", "?limit=many"} {
		e.admin("GET", "/models"+bad, nil).wantError(400, "bad_request")
	}

	// 关闭自动删除时 expires_at 是 0。
	e.admin("PUT", "/settings", map[string]any{"retention_hours": 0}).want(200)
	r := e.admin("GET", "/models", nil).want(200)
	for _, it := range r.fields()["items"].([]any) {
		if it.(map[string]any)["expires_at"] != float64(0) {
			t.Fatalf("expires_at with retention off = %s", r.body)
		}
	}
}

// 禁止名单：禁止后上传和下载都是 blocked，解除后恢复。
func TestBlocklist(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts options
	}{
		{"local", options{}},
		{"s3 direct", options{s3: true, transfer: "direct"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts)
			a, aID := bundle(t, 1500)
			e.mustUpload(aID, a, map[string]any{"name": "Chiffon.vrca"})
			b, bID := bundle(t, 1200)
			e.mustUpload(bID, b, map[string]any{"name": "Manuka.vrca"})

			// 普通删除只是腾空间：之后还能再传。
			del := e.admin("POST", "/models/delete", map[string]any{"ids": []string{bID}}).want(200)
			if del.num("deleted") != 1 || del.num("blocked") != 0 {
				t.Fatalf("delete = %s", del.body)
			}
			e.wantNotDownloadable(bID, "missing")
			e.mustUpload(bID, b, nil)

			// 删除并禁止（其中一个 id 云端没有，也照样进名单）。
			_, cID := bundle(t, 10)
			e.clock.Advance(time.Minute)
			del = e.admin("POST", "/models/delete", map[string]any{
				"ids": []string{aID, cID, aID}, "block": true, "note": "侵权\n投诉",
			}).want(200)
			if del.num("deleted") != 1 || del.num("blocked") != 2 {
				t.Fatalf("delete+block = %s", del.body)
			}
			e.wantNotDownloadable(aID, "blocked")
			e.begin(aID, len(a), nil).wantState("blocked")
			if st := e.status(aID); len(st.fields()) != 2 {
				t.Fatalf("blocked status = %s", st.body)
			}
			if e.fake != nil {
				if _, ok := e.fake.Object("models/" + aID); ok {
					t.Fatal("blocked model is still in the bucket")
				}
			}
			// 另一个模型不受影响。
			if got := e.call("GET", "/api/v1/models/"+bID+"/file", nil).want(200).body; !bytes.Equal(got, b) {
				t.Fatal("content differs")
			}

			list := e.admin("GET", "/blocklist", nil).want(200).fields()["items"].([]any)
			if len(list) != 2 {
				t.Fatalf("blocklist = %v", list)
			}
			var entry map[string]any
			for _, it := range list {
				if it.(map[string]any)["id"] == aID {
					entry = it.(map[string]any)
				}
			}
			if entry == nil || entry["name"] != "Chiffon.vrca" || entry["note"] != "侵权投诉" ||
				entry["blocked_at"] != float64(e.clock.Now().Unix()) || len(entry) != 4 {
				t.Fatalf("blocklist entry = %s", describe(entry))
			}

			// 解除后恢复：回到 missing，可以重新上传和下载。
			rm := e.admin("POST", "/blocklist/remove", map[string]any{"ids": []string{aID, "ffffffffffffffffffffffffffffffff"}}).want(200)
			if rm.num("removed") != 1 {
				t.Fatalf("remove = %s", rm.body)
			}
			e.wantNotDownloadable(aID, "missing")
			e.mustUpload(aID, a, nil)
			e.call("POST", "/api/v1/models/"+aID+"/download", nil).wantState("ready")
			if got := e.call("GET", "/api/v1/models/"+aID+"/file", nil).want(200).body; !bytes.Equal(got, a) {
				t.Fatal("content differs after unblock + re-upload")
			}

			// 手动加一个 id：云端有这个文件的话一并删除，名字从记录里带过来。
			e.admin("POST", "/blocklist", map[string]any{"id": bID, "note": "manual"}).want(200)
			e.wantNotDownloadable(bID, "blocked")
			for _, it := range e.admin("GET", "/blocklist", nil).fields()["items"].([]any) {
				m := it.(map[string]any)
				if m["id"] == bID && (m["name"] != "model.vrca" || m["note"] != "manual") {
					t.Fatalf("manual entry = %s", describe(m))
				}
			}
			if total := e.admin("GET", "/models", nil).num("total"); total != 1 {
				t.Fatalf("models left = %d", total)
			}

			// 正在上传的模型被禁止：会话立刻作废。
			d, dID := bundle(t, 2500)
			uploadID := e.upload(dID, d, nil, 3)
			e.admin("POST", "/blocklist", map[string]any{"id": dID, "name": "手填的名字.vrca"}).want(200)
			if tc.opts.transfer == "direct" {
				e.call("POST", "/api/v1/uploads/"+uploadID+"/parts/3/url", nil).wantError(404, "upload_gone")
			} else {
				e.putPart(uploadID, d, 3).wantError(404, "upload_gone")
			}
			e.complete(uploadID).wantError(404, "upload_gone")
			e.wantNotDownloadable(dID, "blocked")
			if n := e.tempFiles(); n != 0 {
				t.Fatalf("%d temporary file(s) left behind", n)
			}

			e.admin("POST", "/models/delete", map[string]any{"ids": []string{"bad"}}).wantError(400, "bad_id")
			e.admin("POST", "/blocklist", map[string]any{"id": "bad"}).wantError(400, "bad_id")
			e.admin("POST", "/blocklist/remove", map[string]any{"ids": []string{"bad"}}).wantError(400, "bad_id")
			e.admin("POST", "/models/delete", []byte("nope")).wantError(400, "bad_request")
		})
	}
}

// 过期清理：太久没人下载的被删掉；被下载过的从下载那一刻重新计时。
func TestExpiryCleanup(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts options
	}{
		{"local", options{}},
		{"s3", options{s3: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.opts)
			a, aID := bundle(t, 1500)
			e.mustUpload(aID, a, nil)
			b, bID := bundle(t, 1200)
			e.mustUpload(bID, b, nil)
			cleanup := func() (int64, int64) {
				t.Helper()
				r := e.admin("POST", "/cleanup", nil).want(200)
				return r.num("deleted"), r.num("freed_bytes")
			}

			// 71 小时后：都还没到期。b 被下载了一次，续上了期。
			e.clock.Advance(71 * time.Hour)
			if deleted, _ := cleanup(); deleted != 0 {
				t.Fatalf("deleted %d model(s) before anything expired", deleted)
			}
			e.call("POST", "/api/v1/models/"+bID+"/download", nil).wantState("ready")

			// 正好 72 小时：还不算过期（要“超过”才删）。
			e.clock.Advance(time.Hour)
			if deleted, _ := cleanup(); deleted != 0 {
				t.Fatalf("deleted %d model(s) at exactly 72h", deleted)
			}

			// 72 小时零 1 秒：a 过期，b 因为被下载过还在。
			e.clock.Advance(time.Second)
			deleted, freed := cleanup()
			if deleted != 1 || freed != 1500 {
				t.Fatalf("cleanup = deleted %d, freed %d", deleted, freed)
			}
			e.wantNotDownloadable(aID, "missing")
			if got := e.call("GET", "/api/v1/models/"+bID+"/file", nil).want(200).body; !bytes.Equal(got, b) {
				t.Fatal("renewed model is gone or different")
			}
			if e.fake != nil {
				if keys := e.fake.Keys(t); len(keys) != 1 || keys[0] != "models/"+bID {
					t.Fatalf("bucket after cleanup: %v", keys)
				}
			} else if _, err := os.Stat(filepath.Join(e.objectsDir(), aID)); err == nil {
				t.Fatal("expired object is still in storage")
			}
			ov := e.admin("GET", "/overview", nil).want(200)
			if ov.num("model_count") != 1 || ov.num("total_bytes") != 1200 || ov.num("last_cleanup_at") != e.clock.Now().Unix() {
				t.Fatalf("overview = %s", ov.body)
			}
			// 被删掉的可以再传上来。
			e.mustUpload(aID, a, nil)

			// 把保留时长改短：b 上次下载是 1 小时零 1 秒之前。
			if got := e.admin("PUT", "/settings", map[string]any{"retention_hours": 1}).want(200).num("retention_hours"); got != 1 {
				t.Fatalf("settings = %d", got)
			}
			if deleted, freed := cleanup(); deleted != 1 || freed != 1200 {
				t.Fatalf("cleanup after shortening retention = deleted %d, freed %d", deleted, freed)
			}
			e.wantNotDownloadable(bID, "missing")

			// 关掉自动删除：过多久都不删。
			e.admin("PUT", "/settings", map[string]any{"retention_hours": 0}).want(200)
			e.clock.Advance(10000 * time.Hour)
			if deleted, _ := cleanup(); deleted != 0 {
				t.Fatalf("deleted %d model(s) with retention off", deleted)
			}
			e.call("POST", "/api/v1/models/"+aID+"/download", nil).wantState("ready")
		})
	}
}
