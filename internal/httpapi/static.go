package httpapi

import (
	"bytes"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// 管理页不依赖任何外部资源（docs/API.md 第 4 节），这条策略把这一点落到浏览器上：
// 脚本、样式、图片、接口请求都只能来自本站，页面也不能被别的网站嵌进框架里。
// 脚本只允许本站的文件，不允许内联：万一哪里把别人填的文字当成了 HTML，里面的脚本也跑不起来。
// （所以管理页不能用内联的 <script> 和 onclick= 这类属性；内联样式是允许的。）
const adminCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; object-src 'none'; " +
	"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// 常见静态文件的类型。不依赖操作系统的类型表（Windows 上那张表来自注册表，不可靠）。
var staticTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

// serveAdmin 提供管理页的静态文件。segs 是 /admin/ 之后的路径段。
func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request, segs []string) {
	if !allow(w, r, http.MethodGet, http.MethodHead) {
		return
	}
	name := strings.Join(segs, "/")
	if name == "" || strings.HasSuffix(name, "/") {
		name += "index.html"
	}
	// ValidPath 拒绝 ".."、空段、反斜杠开头等一切不规矩的路径；以点开头的文件也不对外提供。
	if !fs.ValidPath(name) || hasDotSegment(name) {
		notFound(w)
		return
	}
	f, err := s.adminFS.Open(name)
	if err != nil {
		notFound(w)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		// 不列目录。
		notFound(w)
		return
	}

	content, ok := f.(io.ReadSeeker)
	if !ok {
		raw, err := io.ReadAll(f)
		if err != nil {
			writeAppError(w, err)
			return
		}
		content = bytes.NewReader(raw)
	}

	ext := strings.ToLower(path.Ext(name))
	ctype := staticTypes[ext]
	if ctype == "" {
		ctype = mime.TypeByExtension(ext)
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Security-Policy", adminCSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	http.ServeContent(w, r, name, time.Time{}, content)
}

func hasDotSegment(name string) bool {
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}
