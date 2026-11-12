package httpapi

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vml-sync-server/internal/app"
	"vml-sync-server/internal/settings"
	"vml-sync-server/internal/storage"
)

// completeTimeout 是一次“完成上传”最多跑多久（直连模式要把内容从对象存储读回来算哈希）。
const completeTimeout = 30 * time.Minute

// fileWriteTimeout 是下载文件时，一块数据最多等多久写不出去就放弃这个连接。
const fileWriteTimeout = 2 * time.Minute

// drainTimeout 是分片请求出错后，最多花多久把没读完的请求体读掉。
const drainTimeout = 30 * time.Second

// maxDrainPart 是分片请求出错后最多读掉多少请求体：一片最大就是 chunk_mb 的上限。
const maxDrainPart = settings.MaxChunkMB << 20

// clientAuthorized 检查 Authorization: Bearer <auth_key>。
func clientAuthorized(r *http.Request, authKey string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	return secureEqual(strings.TrimSpace(h[len(prefix):]), authKey)
}

// clientAPI 处理 /api/v1/ 下除管理接口以外的请求。segs 是 /api/v1/ 之后的路径段。
func (s *Server) clientAPI(w http.ResponseWriter, r *http.Request, segs []string, set *settings.Settings) {
	// 先鉴权再看路径：没有密钥的人连“哪些地址存在”都探不出来。
	if !clientAuthorized(r, set.AuthKey) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or wrong auth key")
		return
	}

	switch {
	case len(segs) == 1 && segs[0] == "ping":
		if allow(w, r, http.MethodGet) {
			writeJSON(w, http.StatusOK, s.app.Ping())
		}

	case len(segs) == 2 && segs[0] == "models":
		if allow(w, r, http.MethodGet) && validModelID(w, segs[1]) {
			s.modelStatus(w, segs[1])
		}
	case len(segs) == 3 && segs[0] == "models" && segs[2] == "upload":
		if allow(w, r, http.MethodPost) && validModelID(w, segs[1]) {
			s.beginUpload(w, r, segs[1])
		}
	case len(segs) == 3 && segs[0] == "models" && segs[2] == "download":
		if allow(w, r, http.MethodPost) && validModelID(w, segs[1]) {
			s.download(w, r, segs[1])
		}
	case len(segs) == 3 && segs[0] == "models" && segs[2] == "file":
		if allow(w, r, http.MethodGet, http.MethodHead) && validModelID(w, segs[1]) {
			s.file(w, r, segs[1])
		}

	case len(segs) == 4 && segs[0] == "uploads" && segs[2] == "parts":
		if allow(w, r, http.MethodPut) {
			s.uploadPart(w, r, segs[1], segs[3])
		}
	case len(segs) == 5 && segs[0] == "uploads" && segs[2] == "parts" && segs[4] == "url":
		if allow(w, r, http.MethodPost) {
			s.partURL(w, segs[1], segs[3])
		}
	case len(segs) == 3 && segs[0] == "uploads" && segs[2] == "complete":
		if allow(w, r, http.MethodPost) {
			s.completeUpload(w, r, segs[1])
		}
	case len(segs) == 3 && segs[0] == "uploads" && segs[2] == "abort":
		if allow(w, r, http.MethodPost) {
			s.app.Abort(segs[1])
			writeJSON(w, http.StatusOK, okBody{OK: true})
		}

	default:
		notFound(w)
	}
}

type okBody struct {
	OK bool `json:"ok"`
}

// validModelID 校验模型 id 的格式；不合法时回 400 bad_id。
func validModelID(w http.ResponseWriter, id string) bool {
	if storage.ValidModelID(id) {
		return true
	}
	writeError(w, http.StatusBadRequest, "bad_id", "model id must be 32 lowercase hex characters")
	return false
}

func (s *Server) modelStatus(w http.ResponseWriter, id string) {
	st, err := s.app.Status(id)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) beginUpload(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Size     *int64 `json:"size"`
		Name     string `json:"name"`
		Uploader string `json:"uploader"`
		Mode     string `json:"mode"`
	}
	if !readJSON(w, r, &req, false) {
		return
	}
	if req.Size == nil || *req.Size <= 0 {
		badRequest(w, "size is required and must be greater than 0")
		return
	}
	res, err := s.app.BeginUpload(r.Context(), id, app.BeginRequest{
		Size:     *req.Size,
		Name:     req.Name,
		Uploader: req.Uploader,
		Mode:     req.Mode,
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, uploadID, part string) {
	n, err := strconv.Atoi(part)
	if err != nil {
		writeAppError(w, app.ErrBadPart)
		return
	}
	// 只要还有字节在流入就一直顺延读超时；流停了超过空闲时限，连接会被掐断。
	idle := s.app.ProxyIdleTimeout()
	rc := http.NewResponseController(w)
	tick := func() { rc.SetReadDeadline(time.Now().Add(idle)) }
	tick()
	// 超时是设在连接上的，处理完要撤掉，免得影响这条连接上的下一个请求。
	defer rc.SetReadDeadline(time.Time{})
	// 一片有多大由会话自己的分片大小决定（设置可能在它开始之后改过），长度由核心逻辑检查；
	// 它最多只读“这一片应有的长度”那么多字节。
	received, err := s.app.WritePart(uploadID, n, r.ContentLength, r.Body, tick)
	if err != nil {
		// 请求体还没读完就回错误的话，正在发送的客户端多半只会看到“连接被重置”而读不到错误内容。
		// 一片的大小有上限，把剩下的读掉再回应。（客户端用了 Expect: 100-continue 的话
		// 它还没开始发，不用读。）
		if r.Header.Get("Expect") == "" && r.ContentLength > 0 && r.ContentLength <= maxDrainPart {
			rc.SetReadDeadline(time.Now().Add(drainTimeout))
			io.CopyN(io.Discard, r.Body, r.ContentLength)
		}
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Received int64 `json:"received"`
	}{received})
}

func (s *Server) partURL(w http.ResponseWriter, uploadID, part string) {
	n, err := strconv.Atoi(part)
	if err != nil {
		writeAppError(w, app.ErrBadPart)
		return
	}
	url, expires, err := s.app.PartURL(uploadID, n)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		URL       string `json:"url"`
		ExpiresIn int    `json:"expires_in"`
	}{url, expires})
}

func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, uploadID string) {
	// 核对和入库不该因为客户端断开就半途而废：用一个不随请求取消的 ctx。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), completeTimeout)
	defer cancel()
	res, err := s.app.Complete(ctx, uploadID)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Mode string `json:"mode"`
	}
	if !readJSON(w, r, &req, true) {
		return
	}
	res, err := s.app.Download(id, req.Mode == settings.TransferProxy)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// file 把模型文件的内容发给客户端，支持 Range 续传。这个接口本身不计下载次数。
func (s *Server) file(w http.ResponseWriter, r *http.Request, id string) {
	m, err := s.app.FileInfo(id)
	if err != nil {
		writeAppError(w, err)
		return
	}
	rng, ok := parseRange(r.Header.Get("Range"), m.Size)
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(m.Size, 10))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "bad_range", "requested range is outside the file")
		return
	}

	var body io.ReadCloser
	if r.Method != http.MethodHead {
		body, err = s.app.OpenFile(r.Context(), id, rng.start, rng.length)
		if err != nil {
			writeAppError(w, err)
			return
		}
		defer body.Close()
	}

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+id+`.vrca"`)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(rng.length, 10))
	status := http.StatusOK
	if rng.partial {
		status = http.StatusPartialContent
		h.Set("Content-Range", "bytes "+strconv.FormatInt(rng.start, 10)+"-"+
			strconv.FormatInt(rng.start+rng.length-1, 10)+"/"+strconv.FormatInt(m.Size, 10))
	}
	w.WriteHeader(status)
	if body == nil {
		return
	}

	// 每写出一块就顺延写超时：正常下载多久都行，卡住不动的连接会被掐断。
	rc := http.NewResponseController(w)
	// 超时是设在连接上的，处理完要撤掉，免得影响这条连接上的下一个响应。
	defer rc.SetWriteDeadline(time.Time{})
	buf := make([]byte, 256<<10)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			rc.SetWriteDeadline(time.Now().Add(fileWriteTimeout))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

// byteRange 是一次文件请求实际要发的字节范围。
type byteRange struct {
	start   int64
	length  int64
	partial bool // true：按 206 回应
}

// parseRange 解析 Range 请求头。只支持单个范围（bytes=N-、bytes=N-M、bytes=-K）；
// 看不懂的写法和多个范围一律当作没带 Range（发整个文件），这是规范允许的。
// 范围完全落在文件之外时返回 false。
func parseRange(header string, size int64) (byteRange, bool) {
	full := byteRange{start: 0, length: size}
	const prefix = "bytes="
	if header == "" || !strings.HasPrefix(header, prefix) {
		return full, true
	}
	spec := strings.TrimSpace(header[len(prefix):])
	if strings.Contains(spec, ",") {
		return full, true
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return full, true
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)

	if first == "" {
		// bytes=-K：最后 K 个字节。
		k, err := strconv.ParseInt(last, 10, 64)
		if err != nil || k < 0 {
			return full, true
		}
		if k == 0 {
			return byteRange{}, false
		}
		k = min(k, size)
		return byteRange{start: size - k, length: k, partial: true}, true
	}

	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return full, true
	}
	end := size - 1
	if last != "" {
		e, err := strconv.ParseInt(last, 10, 64)
		if err != nil || e < start {
			return full, true
		}
		end = min(e, size-1)
	}
	if start >= size {
		return byteRange{}, false
	}
	return byteRange{start: start, length: end - start + 1, partial: true}, true
}
