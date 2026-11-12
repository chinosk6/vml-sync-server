package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vml-sync-server/internal/app"
	"vml-sync-server/internal/meta"
	"vml-sync-server/internal/settings"
	"vml-sync-server/internal/storage"
)

const (
	adminCookie = "vml_admin"
	// sessionTTL 是登录会话的有效期（也是 Cookie 的 Max-Age）。
	sessionTTL = 12 * time.Hour

	// 同一来源在 loginWindow 内输错密码满 loginMaxFailures 次，之后的尝试一律 429，直到最早的那次失败滑出窗口。
	// 登录和“修改密码时核对当前密码”共用这一套计数。
	loginWindow      = 5 * time.Minute
	loginMaxFailures = 5

	// adminHeader 是会改动数据的管理请求必须带的请求头。浏览器不会让别的网站的页面
	// 给跨站请求加自定义请求头，所以它能挡住跨站请求伪造。
	adminHeader = "X-VML-Admin"

	// settingsTimeout 是一次设置修改最多跑多久（换存储时要验证新存储、清空旧存储、对账）。
	settingsTimeout = 10 * time.Minute
)

// adminAPI 处理 /api/v1/admin/ 下的请求。segs 是 admin/ 之后的路径段。
func (s *Server) adminAPI(w http.ResponseWriter, r *http.Request, segs []string, set *settings.Settings) {
	path := strings.Join(segs, "/")

	// 登录、登出、查询会话这三个接口不要求已经登录。
	switch path {
	case "login":
		if allow(w, r, http.MethodPost) {
			s.login(w, r, set)
		}
		return
	case "logout":
		if allow(w, r, http.MethodPost) && hasAdminHeader(w, r) {
			s.logout(w, r, set)
		}
		return
	case "session":
		if allow(w, r, http.MethodGet) {
			writeJSON(w, http.StatusOK, struct {
				Authenticated bool `json:"authenticated"`
			}{s.adminToken(r) != ""})
		}
		return
	}

	// 其余的一律先看会话：没登录的人连“哪些地址存在”都探不出来。
	token := s.adminToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "not logged in")
		return
	}

	switch path {
	case "overview":
		if allow(w, r, http.MethodGet) {
			s.adminOverview(w)
		}
	case "models":
		if allow(w, r, http.MethodGet) {
			s.adminModels(w, r)
		}
	case "models/delete":
		if allow(w, r, http.MethodPost) && hasAdminHeader(w, r) {
			s.adminDelete(w, r)
		}
	case "blocklist":
		if !allow(w, r, http.MethodGet, http.MethodPost) {
			return
		}
		if r.Method == http.MethodGet {
			s.adminBlocklist(w)
		} else if hasAdminHeader(w, r) {
			s.adminBlock(w, r)
		}
	case "blocklist/remove":
		if allow(w, r, http.MethodPost) && hasAdminHeader(w, r) {
			s.adminUnblock(w, r)
		}
	case "settings":
		if !allow(w, r, http.MethodGet, http.MethodPut) {
			return
		}
		if r.Method == http.MethodGet {
			// 取最新的，而不是这个请求开头那一份：免得紧跟在修改后面的读取看到旧值。
			writeJSON(w, http.StatusOK, s.app.Settings().View())
		} else if hasAdminHeader(w, r) {
			s.adminPutSettings(w, r, set, token)
		}
	case "password":
		if allow(w, r, http.MethodPost) && hasAdminHeader(w, r) {
			s.adminPassword(w, r, set, token)
		}
	case "storage/test":
		if allow(w, r, http.MethodPost) && hasAdminHeader(w, r) {
			s.adminTestStorage(w, r)
		}
	case "cleanup":
		if allow(w, r, http.MethodPost) && hasAdminHeader(w, r) {
			s.adminCleanup(w, r)
		}
	case "uploads":
		if allow(w, r, http.MethodGet) {
			writeJSON(w, http.StatusOK, struct {
				Items []app.UploadInfo `json:"items"`
			}{s.app.ActiveUploads()})
		}
	default:
		notFound(w)
	}
}

// hasAdminHeader 检查 X-VML-Admin: 1；没有时回 400 并返回 false。
func hasAdminHeader(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(adminHeader) == "1" {
		return true
	}
	badRequest(w, "requests that change data must carry the header X-VML-Admin: 1")
	return false
}

// adminToken 返回请求带着的有效登录会话的令牌；没有有效会话时返回空串。
func (s *Server) adminToken(r *http.Request) string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	now := s.now()
	// 换过 base_path 之后，浏览器可能同时带着新旧两个路径下的同名 Cookie：逐个看，有一个有效就行。
	for _, c := range r.CookiesNamed(adminCookie) {
		if c.Value == "" {
			continue
		}
		expires, ok := s.sessions[c.Value]
		if !ok {
			continue
		}
		if !now.Before(expires) {
			delete(s.sessions, c.Value)
			continue
		}
		return c.Value
	}
	return ""
}

// loginRetryAfter 返回这个来源还要等多少秒才能再试密码；0 表示现在就可以。
func (s *Server) loginRetryAfter(source string) int {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	now := s.now()
	recent := pruneFailures(s.failures[source], now)
	if len(recent) == 0 {
		delete(s.failures, source)
		return 0
	}
	s.failures[source] = recent
	if len(recent) < loginMaxFailures {
		return 0
	}
	wait := recent[len(recent)-loginMaxFailures].Add(loginWindow).Sub(now)
	return max(int((wait+time.Second-1)/time.Second), 1)
}

// pruneFailures 去掉已经滑出时间窗口的失败记录。
func pruneFailures(times []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(times) && now.Sub(times[i]) >= loginWindow {
		i++
	}
	return times[i:]
}

func (s *Server) recordLoginFailure(source string) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	now := s.now()
	// 来源很多的时候顺手清一遍，免得这张表被人用伪造的来源撑大。
	if len(s.failures) > 4096 {
		for k, v := range s.failures {
			if len(pruneFailures(v, now)) == 0 {
				delete(s.failures, k)
			}
		}
	}
	s.failures[source] = append(pruneFailures(s.failures[source], now), now)
}

// tooManyAttempts 在这个来源被限速时回 429 并返回 true。
func (s *Server) tooManyAttempts(w http.ResponseWriter, source string) bool {
	wait := s.loginRetryAfter(source)
	if wait <= 0 {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	writeJSON(w, http.StatusTooManyRequests, errorBody{
		Error:      "too_many_attempts",
		Message:    "too many wrong passwords; try again later",
		RetryAfter: &wait,
	})
	return true
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, set *settings.Settings) {
	source := clientIP(r, set.TrustForwardedHeaders)
	if s.tooManyAttempts(w, source) {
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req, false) {
		return
	}
	if !s.app.VerifyAdminPassword(req.Password) {
		s.recordLoginFailure(source)
		writeError(w, http.StatusUnauthorized, "bad_password", "wrong password")
		return
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeAppError(w, err)
		return
	}
	token := hex.EncodeToString(raw[:])
	now := s.now()
	s.authMu.Lock()
	for t, expires := range s.sessions {
		if !now.Before(expires) {
			delete(s.sessions, t)
		}
	}
	s.sessions[token] = now.Add(sessionTTL)
	delete(s.failures, source)
	s.authMu.Unlock()

	http.SetCookie(w, sessionCookie(r, set, set.BasePath, token, int(sessionTTL/time.Second)))
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, set *settings.Settings) {
	s.authMu.Lock()
	for _, c := range r.CookiesNamed(adminCookie) {
		delete(s.sessions, c.Value)
	}
	s.authMu.Unlock()
	http.SetCookie(w, sessionCookie(r, set, set.BasePath, "", -1))
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

// sessionCookie 生成登录会话的 Cookie，路径是 basePath 之下；maxAge 为负表示让浏览器删掉它。
func sessionCookie(r *http.Request, set *settings.Settings, basePath, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     adminCookie,
		Value:    value,
		Path:     basePath + "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r, set.TrustForwardedHeaders),
	}
}

func (s *Server) adminOverview(w http.ResponseWriter) {
	ov, err := s.app.Overview()
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := app.ListQuery{Search: q.Get("q"), Sort: "uploaded_at", Desc: true, Limit: 50}

	if v := q.Get("sort"); v != "" {
		if !app.ValidModelSort(v) {
			badRequest(w, "unknown sort field")
			return
		}
		query.Sort = v
	}
	switch q.Get("order") {
	case "", "desc":
	case "asc":
		query.Desc = false
	default:
		badRequest(w, "order must be asc or desc")
		return
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			badRequest(w, "offset must be an integer")
			return
		}
		query.Offset = max(n, 0)
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			badRequest(w, "limit must be an integer")
			return
		}
		query.Limit = min(max(n, 1), 500)
	}

	total, items, err := s.app.ListModels(query)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Total int             `json:"total"`
		Items []app.ModelItem `json:"items"`
	}{total, items})
}

// validIDs 校验一组模型 id；有不合法的就回 400 bad_id 并返回 false。
func validIDs(w http.ResponseWriter, ids []string) bool {
	for _, id := range ids {
		if !validModelID(w, id) {
			return false
		}
	}
	return true
}

func (s *Server) adminDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs   []string `json:"ids"`
		Block bool     `json:"block"`
		Note  string   `json:"note"`
	}
	if !readJSON(w, r, &req, false) || !validIDs(w, req.IDs) {
		return
	}
	deleted, blocked, err := s.app.DeleteModels(r.Context(), req.IDs, req.Block, req.Note)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Deleted int `json:"deleted"`
		Blocked int `json:"blocked"`
	}{deleted, blocked})
}

func (s *Server) adminBlocklist(w http.ResponseWriter) {
	items, err := s.app.Blocklist()
	if err != nil {
		writeAppError(w, err)
		return
	}
	if items == nil {
		items = []meta.Blocked{}
	}
	writeJSON(w, http.StatusOK, struct {
		Items []meta.Blocked `json:"items"`
	}{items})
}

func (s *Server) adminBlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if !readJSON(w, r, &req, false) || !validModelID(w, req.ID) {
		return
	}
	if err := s.app.BlockID(r.Context(), req.ID, req.Name, req.Note); err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

func (s *Server) adminUnblock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !readJSON(w, r, &req, false) {
		return
	}
	for _, id := range req.IDs {
		if !storage.ValidModelID(id) {
			writeError(w, http.StatusBadRequest, "bad_id", "model id must be 32 lowercase hex characters")
			return
		}
	}
	removed, err := s.app.Unblock(req.IDs)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Removed int `json:"removed"`
	}{removed})
}

// adminPutSettings 处理设置的局部更新（docs/API.md 3.5）。
func (s *Server) adminPutSettings(w http.ResponseWriter, r *http.Request, set *settings.Settings, token string) {
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		badRequest(w, "request body is empty")
		return
	}
	var patch settings.Patch
	if err := json.Unmarshal(raw, &patch); err != nil {
		// 某一项的类型不对（比如该是整数的给了字符串、小数）：指出是哪一项。
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			writeJSON(w, http.StatusBadRequest, errorBody{
				Error:   "invalid_settings",
				Message: typeErr.Field + ": must be a JSON " + jsonTypeName(typeErr),
				Field:   typeErr.Field,
			})
			return
		}
		badRequest(w, "request body is not valid JSON")
		return
	}

	// 换存储时“清空旧的、切到新的”不该因为页面关掉就做到一半停下。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), settingsTimeout)
	defer cancel()
	next, err := s.app.UpdateSettings(ctx, patch)
	if err != nil {
		writeAppError(w, err)
		return
	}

	if next.BasePath != set.BasePath {
		// 入口换了地方：在同一个响应里把会话 Cookie 搬到新路径下，旧路径下的作废。
		// 这个响应照常返回；从下一个请求起只认新前缀。
		s.authMu.Lock()
		expires := s.sessions[token]
		s.authMu.Unlock()
		remaining := max(int(expires.Sub(s.now())/time.Second), 1)
		after := next
		http.SetCookie(w, sessionCookie(r, &after, set.BasePath, "", -1))
		http.SetCookie(w, sessionCookie(r, &after, next.BasePath, token, remaining))
	}
	writeJSON(w, http.StatusOK, next.View())
}

// jsonTypeName 说明一个设置项应该是什么类型。
func jsonTypeName(e *json.UnmarshalTypeError) string {
	switch e.Type.Kind().String() {
	case "bool":
		return "boolean"
	case "string":
		return "string"
	case "struct":
		return "object"
	default:
		return "whole number"
	}
}

// adminPassword 处理“修改管理员密码”（docs/API.md 3.7）。
func (s *Server) adminPassword(w http.ResponseWriter, r *http.Request, set *settings.Settings, token string) {
	// 核对当前密码和登录一样计入限速：拿到一个登录会话的人不能借这个接口无限次地猜密码。
	source := clientIP(r, set.TrustForwardedHeaders)
	if s.tooManyAttempts(w, source) {
		return
	}
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &req, false) {
		return
	}
	if !s.app.VerifyAdminPassword(req.Current) {
		s.recordLoginFailure(source)
		writeError(w, http.StatusUnauthorized, "bad_password", "current password is wrong")
		return
	}
	if err := s.app.ChangeAdminPassword(req.New); err != nil {
		writeAppError(w, err)
		return
	}
	// 别的登录会话全部作废，发起请求的这个保留。
	s.authMu.Lock()
	for t := range s.sessions {
		if t != token {
			delete(s.sessions, t)
		}
	}
	delete(s.failures, source)
	s.authMu.Unlock()
	writeJSON(w, http.StatusOK, okBody{OK: true})
}

// adminTestStorage 处理“测试存储设置”（docs/API.md 3.8）：不保存任何东西。
func (s *Server) adminTestStorage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Storage *settings.StoragePatch `json:"storage"`
	}
	if !readJSON(w, r, &req, false) {
		return
	}
	ok, message := s.app.TestStorage(r.Context(), req.Storage)
	if ok {
		writeJSON(w, http.StatusOK, okBody{OK: true})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}{false, message})
}

func (s *Server) adminCleanup(w http.ResponseWriter, r *http.Request) {
	deleted, freed, err := s.app.Cleanup(r.Context())
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Deleted    int   `json:"deleted"`
		FreedBytes int64 `json:"freed_bytes"`
	}{deleted, freed})
}
