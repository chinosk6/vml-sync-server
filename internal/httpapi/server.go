// Package httpapi 是 HTTP 层：路由、鉴权、请求和响应的格式（docs/API.md）。
package httpapi

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"vml-sync-server/internal/app"
	"vml-sync-server/internal/settings"
)

// maxJSONBody 是 JSON 请求体的大小上限。
const maxJSONBody = 64 << 10

// jsonReadTimeout 是读完一个 JSON 请求体最多等多久。
const jsonReadTimeout = 30 * time.Second

// Options 是创建 Server 时的可选项。
type Options struct {
	// Now 返回当前时间；留空用 time.Now。测试里用它拨动时间。
	Now func() time.Time
	// Quiet 为 true 时不打访问日志（测试用）。
	Quiet bool
}

// Server 实现 http.Handler：base_path 之下是接口和管理页，之外一律 404。
//
// 它自己不保存任何设置：每个请求开头从 App 取一次当前的设置（base_path、auth_key、
// 是否信任转发头……），所以在管理页里改完设置，下一个请求就按新的来。
type Server struct {
	app     *app.App
	adminFS fs.FS
	now     func() time.Time
	quiet   bool

	// 管理员登录会话和登录失败记录，都只存在内存里。
	authMu   sync.Mutex
	sessions map[string]time.Time   // 令牌 → 过期时间
	failures map[string][]time.Time // 来源地址 → 最近几次登录失败的时间
}

// New 创建 Server。adminFS 是管理页的静态文件（以 index.html 所在目录为根）。
func New(a *app.App, adminFS fs.FS, opts Options) *Server {
	s := &Server{
		app:      a,
		adminFS:  adminFS,
		now:      opts.Now,
		quiet:    opts.Quiet,
		sessions: make(map[string]time.Time),
		failures: make(map[string][]time.Time),
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// recorder 记下响应的状态码和字节数，给访问日志用。
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Unwrap 让 http.ResponseController 能找到底层的连接（设置读写超时要用）。
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &recorder{ResponseWriter: w}
	h := rec.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")

	// 整个请求用同一份设置。
	set := s.app.Settings()
	s.route(rec, r, &set)
	drainRequestBody(rec, r)

	if !s.quiet {
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		// 只记路径，不记查询串和任何请求头：密钥、密码、临时地址都不会进日志。
		log.Printf("%s %s %q %d %d %s", clientIP(r, set.TrustForwardedHeaders), r.Method, r.URL.Path, status, rec.bytes,
			time.Since(start).Round(time.Millisecond))
	}
}

// 处理函数没读完的请求体，最多再读这么多、等这么久。
const (
	maxDrainBytes     = 256 << 10
	drainBodyDeadline = 5 * time.Second
)

// drainRequestBody 把处理函数没读的请求体读掉。
//
// 有些接口不看请求体（complete、abort、cleanup……），而客户端可能照样带一个 "{}"。
// 请求带 Connection: close 时，标准库不会替我们读掉它；带着没读的数据关连接，
// 对方收到的是“连接被重置”，连已经发出去的响应都可能读不到。所以这里统一读掉。
func drainRequestBody(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return
	}
	// 客户端在等 100-continue 的话，它还没开始发请求体，没有东西可读。
	if r.Header.Get("Expect") != "" {
		return
	}
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(drainBodyDeadline))
	io.CopyN(io.Discard, r.Body, maxDrainBytes)
	rc.SetReadDeadline(time.Time{})
}

// route 按路径分发请求。
func (s *Server) route(w http.ResponseWriter, r *http.Request, set *settings.Settings) {
	p := r.URL.Path
	if base := set.BasePath; base != "" {
		if p != base && !strings.HasPrefix(p, base+"/") {
			outside(w)
			return
		}
		p = p[len(base):]
	}
	if !strings.HasPrefix(p, "/") {
		notFound(w)
		return
	}
	segs := strings.Split(p[1:], "/")

	switch {
	case len(segs) >= 3 && segs[0] == "api" && segs[1] == "v1" && segs[2] == "admin":
		s.adminAPI(w, r, segs[3:], set)
	case len(segs) >= 2 && segs[0] == "api" && segs[1] == "v1":
		s.clientAPI(w, r, segs[2:], set)
	case len(segs) == 1 && segs[0] == "admin":
		// 用相对地址重定向：不管 base_path 是什么、前面有没有改写路径的反向代理都对。
		target := "admin/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
	case len(segs) >= 2 && segs[0] == "admin":
		s.serveAdmin(w, r, segs[1:])
	default:
		notFound(w)
	}
}

// outside 回应 base_path 之外的请求：和一个什么都没有的服务器一样，不透露任何信息。
func outside(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, "404 page not found\n")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		log.Printf("error: encode response: %v", err)
		status, raw = http.StatusInternalServerError, []byte(`{"error":"internal","message":"internal server error"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(raw)
}

// errorBody 是出错时的响应体。后三项只在个别错误里出现。
type errorBody struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	RetryAfter *int   `json:"retry_after,omitempty"`
	Field      string `json:"field,omitempty"`
	ModelCount *int   `json:"model_count,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: code, Message: message})
}

func notFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "not found")
}

func badRequest(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, "bad_request", message)
}

// writeAppError 把核心逻辑返回的错误写成响应。
func writeAppError(w http.ResponseWriter, err error) {
	var ae *app.Error
	if errors.As(err, &ae) {
		writeJSON(w, ae.Status, errorBody{Error: ae.Code, Message: ae.Message, Field: ae.Field, ModelCount: ae.ModelCount})
		return
	}
	log.Printf("error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal server error")
}

// allow 检查请求方法；不在允许之列时回 405 并返回 false。
func allow(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	return false
}

// readBody 读出请求体（最多 64 KB）。出错时已经写好响应并返回 false。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	// 请求体很小，给个总时限，免得被慢速连接长期占着。超时是设在连接上的，读完就撤掉：
	// 留着的话，它会在后面的处理过程中到点，被当成“客户端断开了”。
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(jsonReadTimeout))
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	rc.SetReadDeadline(time.Time{})
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body is too large")
		} else {
			badRequest(w, "cannot read request body")
		}
		return nil, false
	}
	return raw, true
}

// readJSON 读取并解析 JSON 请求体。出错时已经写好响应并返回 false。
// allowEmpty 为 true 时空请求体不算错（into 保持原样）。
func readJSON(w http.ResponseWriter, r *http.Request, into any, allowEmpty bool) bool {
	raw, ok := readBody(w, r)
	if !ok {
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if allowEmpty {
			return true
		}
		badRequest(w, "request body is empty")
		return false
	}
	if err := json.Unmarshal(raw, into); err != nil {
		badRequest(w, "request body is not valid JSON for this endpoint")
		return false
	}
	return true
}

// secureEqual 用常数时间比较两个字符串。先各自取哈希，这样连长度也不会从耗时里漏出去。
func secureEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// clientIP 返回请求的来源地址。trusted（trust_forwarded_headers 打开）时取 X-Forwarded-For 的最后一项：
// 那是紧挨着本服务的反向代理亲眼看到的地址，客户端自己伪造不了。
func clientIP(r *http.Request, trusted bool) string {
	if trusted {
		if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
			parts := strings.Split(values[len(values)-1], ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isHTTPS 报告这个请求是不是经 HTTPS 来的（直接的，或者受信任的反向代理报告的）。
func isHTTPS(r *http.Request, trusted bool) bool {
	if r.TLS != nil {
		return true
	}
	if trusted {
		proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
		return strings.EqualFold(proto, "https")
	}
	return false
}
