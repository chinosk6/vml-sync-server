package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"vml-sync-server/internal/meta"
	"vml-sync-server/internal/settings"
	"vml-sync-server/internal/storage"
)

// upload 是一个上传会话。会话只存在内存里，服务重启后全部作废。
type upload struct {
	// 创建后不再变的字段。
	id        string // 上传会话 id
	modelID   string
	name      string
	uploader  string
	size      int64
	mode      string
	chunk     int64
	partCount int
	startedAt int64
	path      string // 中转模式的临时文件

	// 会话开始时的存储。设置可以随时改，但一个会话从头到尾用同一个存储、同一个分片大小。
	store  storage.Store
	direct storage.Direct
	gen    uint64 // 开始时的存储代数；登记时对不上就说明存储已经换了

	lastActive atomic.Int64 // 最近一次活动的时间（UnixNano）

	// completeMu 让同一个会话的 complete 请求排队。
	completeMu sync.Mutex

	// ioMu：往临时文件里写一块数据时持读锁；封口和关闭文件时持写锁。
	// 加锁顺序：ioMu 在 mu 之前。
	ioMu sync.RWMutex

	mu           sync.Mutex
	file         *os.File
	done         []bool // 中转模式：每一片是否已经完整收到
	received     int64  // 中转模式：已完整收到的分片的总大小
	maxRequested int    // 直连模式：签发过地址的最大片号
	handle       string // 直连模式：对象存储那边的分片上传句柄（拼好之后清空）
	staged       bool   // 直连模式：分片已经拼成暂存对象
	sealed       bool   // 中转模式：分片已收齐、正在核对，不再接受写入
	finishing    bool   // 正在执行 complete
	cancelled    bool   // 收尾期间被要求作废（收尾的一方负责善后）
	closed       bool   // 会话已经作废
	result       *CompleteResult
}

// partLen 返回第 n 片（从 1 开始）应有的长度。
func (u *upload) partLen(n int) int64 {
	if n < u.partCount {
		return u.chunk
	}
	return u.size - int64(u.partCount-1)*u.chunk
}

func (u *upload) touch(now time.Time) { u.lastActive.Store(now.UnixNano()) }

// receivedBytes 返回已经收到的字节数，只用于显示进度。
func (u *upload) receivedBytes() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mode == settings.TransferProxy {
		return u.received
	}
	if u.staged {
		return u.size
	}
	// 直连模式看不到真实进度：请求第 n 片的地址即视为前 n-1 片已完成。
	if u.maxRequested <= 1 {
		return 0
	}
	return min(int64(u.maxRequested-1)*u.chunk, u.size)
}

// markClosed 把会话标记为作废。返回 true 表示调用方接下来要负责摘除会话并释放资源。
// 会话正在收尾时不能直接关：cancel 为 true 就给它留个记号，由收尾的一方善后。
func (u *upload) markClosed(cancel bool) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return false
	}
	if u.finishing {
		if cancel {
			u.cancelled = true
		}
		return false
	}
	u.closed = true
	return true
}

// BeginRequest 是“开始上传”的请求内容。
type BeginRequest struct {
	Size     int64
	Name     string
	Uploader string
	Mode     string
}

// BeginResult 是“开始上传”的响应（docs/API.md 2.3）。
type BeginResult struct {
	State        string `json:"state"`
	MaxFileBytes *int64 `json:"max_file_bytes,omitempty"`
	RetryAfter   *int   `json:"retry_after,omitempty"`
	Size         *int64 `json:"size,omitempty"`
	Received     *int64 `json:"received,omitempty"`
	UploadID     string `json:"upload_id,omitempty"`
	Mode         string `json:"mode,omitempty"`
	ChunkBytes   *int64 `json:"chunk_bytes,omitempty"`
	PartCount    *int   `json:"part_count,omitempty"`
}

func busy(size, received int64) BeginResult {
	return BeginResult{State: StateBusy, RetryAfter: ptr(busyRetryAfter), Size: ptr(size), Received: ptr(received)}
}

// BeginUpload 为模型 id 开一个上传会话，或者说明为什么不用 / 不能传。
func (a *App) BeginUpload(ctx context.Context, id string, req BeginRequest) (BeginResult, error) {
	name := CleanText(req.Name, MaxNameChars)
	uploader := CleanText(req.Uploader, MaxUploaderChars)

	a.mu.Lock()
	// 在锁里取设置：换存储也是在这把锁里换的，所以会话要么建在换之前（会被一并作废），要么用的就是新存储。
	st, err := a.current()
	if err != nil {
		a.mu.Unlock()
		return BeginResult{}, err
	}
	blocked, err := a.db.IsBlocked(id)
	if err != nil {
		a.mu.Unlock()
		return BeginResult{}, internalError("begin upload", err)
	}
	if blocked {
		a.mu.Unlock()
		return BeginResult{State: StateBlocked}, nil
	}
	_, exists, err := a.db.Model(id)
	if err != nil {
		a.mu.Unlock()
		return BeginResult{}, internalError("begin upload", err)
	}
	if exists {
		a.mu.Unlock()
		return BeginResult{State: StateReady}, nil
	}
	if req.Size > st.maxFileBytes {
		a.mu.Unlock()
		return BeginResult{State: StateTooLarge, MaxFileBytes: ptr(st.maxFileBytes)}, nil
	}
	if other := a.byModel[id]; other != nil {
		a.mu.Unlock()
		return busy(other.size, other.receivedBytes()), nil
	}
	if len(a.uploads) >= st.set.MaxActiveUploads {
		a.mu.Unlock()
		return busy(0, 0), nil
	}
	if st.maxTotalBytes > 0 {
		_, used, err := a.db.Usage()
		if err != nil {
			a.mu.Unlock()
			return BeginResult{}, internalError("begin upload", err)
		}
		// 正在传的也算进去，免得几个上传同时进来一起超限。
		for _, other := range a.uploads {
			used += other.size
		}
		if used+req.Size > st.maxTotalBytes {
			a.mu.Unlock()
			return BeginResult{State: StateStorageFull}, nil
		}
	}

	mode := st.set.Transfer
	if mode == settings.TransferDirect && (req.Mode == settings.TransferProxy || st.direct == nil) {
		// 客户端直连对象存储失败后的退路：这一次改走中转。
		mode = settings.TransferProxy
	}
	u, err := a.newUpload(st, id, name, uploader, req.Size, mode)
	if err != nil {
		a.mu.Unlock()
		return BeginResult{}, internalError("begin upload", err)
	}
	a.uploads[u.id] = u
	a.byModel[id] = u
	a.mu.Unlock()

	if mode == settings.TransferDirect {
		// 会话已经占住了位置，对象存储的调用放在锁外面做。
		handle, err := u.direct.Begin(ctx, u.id)
		if err != nil {
			a.drop(u, true)
			return BeginResult{}, internalError("begin direct upload", err)
		}
		u.mu.Lock()
		u.handle = handle
		closed := u.closed
		u.mu.Unlock()
		if closed {
			// 等对象存储回话的工夫会话被作废了（比如管理员正好禁止了这个模型，或者换了存储）。
			bg, cancel := backgroundContext(time.Minute)
			_ = u.direct.Abort(bg, u.id, handle)
			cancel()
			if blocked, _ := a.db.IsBlocked(id); blocked {
				return BeginResult{State: StateBlocked}, nil
			}
			return BeginResult{}, ErrUploadGone
		}
	}

	return BeginResult{
		State:      StateUpload,
		UploadID:   u.id,
		Mode:       mode,
		ChunkBytes: ptr(u.chunk),
		PartCount:  ptr(u.partCount),
	}, nil
}

// newUpload 创建会话对象；中转模式下同时建好临时文件。调用方持有 a.mu。
func (a *App) newUpload(st *state, modelID, name, uploader string, size int64, mode string) (*upload, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	now := a.now()
	chunk := st.chunkBytes
	u := &upload{
		id:        hex.EncodeToString(raw[:]),
		modelID:   modelID,
		name:      name,
		uploader:  uploader,
		size:      size,
		mode:      mode,
		chunk:     chunk,
		partCount: int((size + chunk - 1) / chunk),
		startedAt: now.Unix(),
		store:     st.store,
		direct:    st.direct,
		gen:       st.gen,
	}
	u.touch(now)
	if mode == settings.TransferProxy {
		u.path = filepath.Join(a.uploadDir, u.id+".part")
		f, err := os.OpenFile(u.path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		u.file = f
		u.done = make([]bool, u.partCount)
	}
	return u, nil
}

func (a *App) lookup(uploadID string) *upload {
	if !storage.ValidUploadID(uploadID) {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.uploads[uploadID]
}

// detachLocked 把会话从表里摘掉。调用方持有 a.mu。
func (a *App) detachLocked(u *upload) {
	if a.uploads[u.id] == u {
		delete(a.uploads, u.id)
	}
	if a.byModel[u.modelID] == u {
		delete(a.byModel, u.modelID)
	}
}

// drop 作废一个会话并清掉它的临时数据。会话正在收尾时：cancel 为 true 就让收尾的一方
// 放弃并善后，为 false 就不打扰它。
func (a *App) drop(u *upload, cancel bool) {
	if !u.markClosed(cancel) {
		return
	}
	a.mu.Lock()
	a.detachLocked(u)
	a.mu.Unlock()
	a.free(u)
}

// free 释放一个已经作废的会话占用的资源：临时文件、对象存储上的分片上传或暂存对象。
func (a *App) free(u *upload) {
	// 写锁保证没有正在进行的写入；之后的写入会看到 closed 而放弃。
	u.ioMu.Lock()
	u.mu.Lock()
	f := u.file
	u.file = nil
	handle, staged := u.handle, u.staged
	u.handle, u.staged = "", false
	u.mu.Unlock()
	u.ioMu.Unlock()

	if f != nil {
		f.Close()
	}
	if u.path != "" {
		if err := os.Remove(u.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("warning: cannot remove temporary file of upload %s: %v", u.id, err)
		}
	}
	if u.direct == nil || (!staged && handle == "") {
		return
	}
	ctx, cancel := backgroundContext(time.Minute)
	defer cancel()
	if staged {
		if err := u.direct.DiscardStaged(ctx, u.id); err != nil {
			log.Printf("warning: cannot delete staged object of upload %s: %v", u.id, err)
		}
	}
	if handle != "" {
		if err := u.direct.Abort(ctx, u.id, handle); err != nil {
			log.Printf("warning: cannot abort multipart upload of upload %s: %v", u.id, err)
		}
	}
}

var copyBufPool = sync.Pool{New: func() any {
	b := make([]byte, 256<<10)
	return &b
}}

// WritePart 接收中转模式的第 n 片。contentLength 是请求声明的长度，body 是请求体。
// 每收到一块数据调一次 tick（HTTP 层用它顺延读超时）。返回已完整收到的总字节数。
func (a *App) WritePart(uploadID string, n int, contentLength int64, body io.Reader, tick func()) (int64, error) {
	if _, err := a.current(); err != nil {
		return 0, err
	}
	u := a.lookup(uploadID)
	if u == nil {
		return 0, ErrUploadGone
	}
	if u.mode != settings.TransferProxy {
		return 0, ErrWrongMode
	}
	if contentLength > u.chunk {
		return 0, ErrPartTooLarge
	}
	if n < 1 || n > u.partCount {
		return 0, ErrBadPart
	}
	want := u.partLen(n)
	if contentLength != want {
		return 0, ErrBadPart
	}
	offset := int64(n-1) * u.chunk

	// 这一片要被（重新）写入了：在写完之前它不算已收到。
	u.mu.Lock()
	if u.closed || u.sealed {
		u.mu.Unlock()
		return 0, ErrUploadGone
	}
	if u.done[n-1] {
		u.done[n-1] = false
		u.received -= want
	}
	u.mu.Unlock()

	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	buf := *bufp

	var got int64
	for got < want {
		nr, rerr := body.Read(buf[:min(int64(len(buf)), want-got)])
		if nr > 0 {
			if err := u.writeAt(buf[:nr], offset+got); err != nil {
				if errors.Is(err, ErrUploadGone) {
					return 0, ErrUploadGone
				}
				return 0, internalError("write upload part", err)
			}
			got += int64(nr)
			// 有字节在流入：会话不算空闲。
			u.touch(a.now())
			if tick != nil {
				tick()
			}
		}
		if rerr != nil {
			break
		}
	}
	if got != want {
		// 客户端中途断开或者发的比声明的少。
		return 0, &Error{Status: 400, Code: "bad_part", Message: "request body is shorter than the part length"}
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.sealed {
		return 0, ErrUploadGone
	}
	if !u.done[n-1] {
		u.done[n-1] = true
		u.received += want
	}
	return u.received, nil
}

// writeAt 往临时文件的指定位置写一块数据。
func (u *upload) writeAt(p []byte, off int64) error {
	u.ioMu.RLock()
	defer u.ioMu.RUnlock()
	u.mu.Lock()
	f := u.file
	dead := u.closed || u.sealed
	u.mu.Unlock()
	if dead || f == nil {
		return ErrUploadGone
	}
	_, err := f.WriteAt(p, off)
	return err
}

// PartURL 为直连模式的第 n 片签发一个临时上传地址，返回地址和有效期（秒）。
func (a *App) PartURL(uploadID string, n int) (string, int, error) {
	if _, err := a.current(); err != nil {
		return "", 0, err
	}
	u := a.lookup(uploadID)
	if u == nil {
		return "", 0, ErrUploadGone
	}
	if u.mode != settings.TransferDirect {
		return "", 0, ErrWrongMode
	}
	if n < 1 || n > u.partCount {
		return "", 0, ErrBadPart
	}
	u.mu.Lock()
	if u.closed || u.handle == "" {
		u.mu.Unlock()
		return "", 0, ErrUploadGone
	}
	handle := u.handle
	if n > u.maxRequested {
		u.maxRequested = n
	}
	u.mu.Unlock()
	u.touch(a.now())

	ttl := a.st.Load().set.DirectURLTTLSeconds
	url, err := u.direct.PartURL(u.id, handle, n, time.Duration(ttl)*time.Second)
	if err != nil {
		return "", 0, internalError("sign part url", err)
	}
	return url, ttl, nil
}

// CompleteResult 是“完成上传”的响应（docs/API.md 2.5）。
type CompleteResult struct {
	State   string `json:"state"`
	ID      string `json:"id,omitempty"`
	Missing []int  `json:"missing,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func rejected(reason string) CompleteResult {
	return CompleteResult{State: StateRejected, Reason: reason}
}

// Complete 核对一个上传会话收到的内容，全部通过才登记为可下载。
// 这一步可能要跑很久，而且不该因为请求方断开就半途而废，所以调用方应当传一个不随请求取消的 ctx。
func (a *App) Complete(ctx context.Context, uploadID string) (CompleteResult, error) {
	if _, err := a.current(); err != nil {
		return CompleteResult{}, err
	}
	u := a.lookup(uploadID)
	if u == nil {
		// 上一次 complete 其实已经跑完、只是响应没送到的话，把当时的结果再给一遍。
		if res, ok := a.recall(uploadID); ok {
			return res, nil
		}
		return CompleteResult{}, ErrUploadGone
	}

	u.completeMu.Lock()
	defer u.completeMu.Unlock()

	u.mu.Lock()
	if u.result != nil {
		// 排队期间前一个 complete 已经出了结果。
		res := *u.result
		u.mu.Unlock()
		return res, nil
	}
	if u.closed || u.cancelled {
		u.mu.Unlock()
		return CompleteResult{}, ErrUploadGone
	}
	u.finishing = true
	u.mu.Unlock()
	u.touch(a.now())

	var (
		res  CompleteResult
		over bool // 会话到此结束（ready / rejected / 已经没法继续）
		err  error
	)
	if u.mode == settings.TransferProxy {
		res, over, err = a.completeProxy(ctx, u)
	} else {
		res, over, err = a.completeDirect(ctx, u)
	}

	if over && err == nil {
		u.mu.Lock()
		u.result = &res
		u.mu.Unlock()
		a.remember(u.id, res)
	}
	u.touch(a.now())

	u.mu.Lock()
	u.finishing = false
	end := over || u.cancelled
	if end {
		u.closed = true
	} else {
		u.sealed = false
	}
	u.mu.Unlock()
	if end {
		a.mu.Lock()
		a.detachLocked(u)
		a.mu.Unlock()
		a.free(u)
	}
	return res, err
}

func (a *App) remember(uploadID string, res CompleteResult) {
	a.mu.Lock()
	a.finished[uploadID] = finished{result: res, expires: a.now().Add(finishedTTL)}
	a.mu.Unlock()
}

func (a *App) recall(uploadID string) (CompleteResult, bool) {
	if !storage.ValidUploadID(uploadID) {
		return CompleteResult{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	f, ok := a.finished[uploadID]
	if !ok || a.now().After(f.expires) {
		return CompleteResult{}, false
	}
	return f.result, true
}

// completeProxy 核对中转模式收到的临时文件，通过后交给存储。
func (a *App) completeProxy(ctx context.Context, u *upload) (CompleteResult, bool, error) {
	// 封口：拿到写锁说明没有正在进行的写入；分片齐了就不再接受新的写入。
	u.ioMu.Lock()
	u.mu.Lock()
	var missing []int
	for i, ok := range u.done {
		if !ok {
			missing = append(missing, i+1)
		}
	}
	if len(missing) == 0 {
		u.sealed = true
	}
	f := u.file
	u.mu.Unlock()
	u.ioMu.Unlock()

	if len(missing) > 0 {
		return CompleteResult{State: StateIncomplete, Missing: missing}, false, nil
	}
	if f == nil {
		return CompleteResult{}, true, internalError("complete upload", errors.New("temporary file is not open"))
	}

	sum, head, n, err := digest(io.NewSectionReader(f, 0, math.MaxInt64))
	if err != nil {
		return CompleteResult{}, false, internalError("hash uploaded file", err)
	}
	if reason := check(u, n, head, sum, true); reason != "" {
		return rejected(reason), true, nil
	}

	unlock := a.lockID(u.modelID)
	defer unlock()
	if a.isCancelled(u) {
		return CompleteResult{}, true, ErrUploadGone
	}

	// 打开着的文件在 Windows 上不能改名：先把句柄关掉。
	u.mu.Lock()
	u.file = nil
	u.mu.Unlock()
	f.Close()

	if err := u.store.PutFile(ctx, u.modelID, u.path, u.size, sum); err != nil {
		// 存储暂时出了问题：留着会话和临时文件，客户端可以再调一次 complete。
		reopened, oerr := os.OpenFile(u.path, os.O_RDWR, 0)
		if oerr != nil {
			return CompleteResult{}, true, internalError("store uploaded file", err)
		}
		u.mu.Lock()
		u.file = reopened
		u.mu.Unlock()
		return CompleteResult{}, false, internalError("store uploaded file", err)
	}
	return a.register(u)
}

// completeDirect 核对直连模式传到对象存储的内容，通过后复制到最终位置。
func (a *App) completeDirect(ctx context.Context, u *upload) (CompleteResult, bool, error) {
	u.mu.Lock()
	handle, staged := u.handle, u.staged
	u.mu.Unlock()

	if !staged {
		if handle == "" {
			return CompleteResult{}, true, ErrUploadGone
		}
		parts, err := u.direct.Parts(ctx, u.id, handle)
		if err != nil {
			if errors.Is(err, storage.ErrNotExist) {
				// 对象存储那边的分片上传已经不在了（比如被桶的生命周期规则清掉）。
				return CompleteResult{}, true, ErrUploadGone
			}
			return CompleteResult{}, false, internalError("list uploaded parts", err)
		}
		have := make(map[int]storage.Part, len(parts))
		for _, p := range parts {
			have[p.Number] = p
		}
		var missing []int
		ordered := make([]storage.Part, 0, u.partCount)
		for n := 1; n <= u.partCount; n++ {
			p, ok := have[n]
			if !ok {
				missing = append(missing, n)
				continue
			}
			ordered = append(ordered, p)
		}
		if len(missing) > 0 {
			return CompleteResult{State: StateIncomplete, Missing: missing}, false, nil
		}
		for _, p := range ordered {
			if p.Size != u.partLen(p.Number) {
				return rejected(ReasonSizeMismatch), true, nil
			}
		}
		if err := u.direct.Assemble(ctx, u.id, handle, ordered); err != nil {
			if errors.Is(err, storage.ErrNotExist) {
				return CompleteResult{}, true, ErrUploadGone
			}
			return CompleteResult{}, false, internalError("assemble uploaded parts", err)
		}
		u.mu.Lock()
		u.staged = true
		u.handle = ""
		u.mu.Unlock()
	}

	size, err := u.direct.StatStaged(ctx, u.id)
	if err != nil {
		return CompleteResult{}, false, internalError("stat staged object", err)
	}
	if size != u.size {
		return rejected(ReasonSizeMismatch), true, nil
	}

	var (
		sum  []byte
		head []byte
		n    = size
		// 核对与否按现在的设置来（不是会话开始时的）。
		verify = a.st.Load().set.VerifyDirectUploads
	)
	if verify {
		// 把内容读回来算哈希。
		rc, err := u.direct.OpenStaged(ctx, u.id, 0, -1)
		if err != nil {
			return CompleteResult{}, false, internalError("read back staged object", err)
		}
		sum, head, n, err = digest(rc)
		rc.Close()
		if err != nil {
			return CompleteResult{}, false, internalError("read back staged object", err)
		}
	} else {
		// 配置里关掉了哈希核对：至少看一眼文件头。
		rc, err := u.direct.OpenStaged(ctx, u.id, 0, int64(len(bundleMagic)))
		if err != nil {
			return CompleteResult{}, false, internalError("read staged object header", err)
		}
		head, err = io.ReadAll(io.LimitReader(rc, int64(len(bundleMagic))))
		rc.Close()
		if err != nil {
			return CompleteResult{}, false, internalError("read staged object header", err)
		}
	}
	if reason := check(u, n, head, sum, verify); reason != "" {
		return rejected(reason), true, nil
	}

	unlock := a.lockID(u.modelID)
	defer unlock()
	if a.isCancelled(u) {
		return CompleteResult{}, true, ErrUploadGone
	}
	if err := u.direct.Promote(ctx, u.id, u.modelID); err != nil {
		return CompleteResult{}, false, internalError("promote staged object", err)
	}
	u.mu.Lock()
	u.staged = false
	u.mu.Unlock()
	return a.register(u)
}

func (a *App) isCancelled(u *upload) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cancelled
}

// register 把刚放进存储的模型登记为可下载。调用方持有这个模型 id 的锁。
func (a *App) register(u *upload) (CompleteResult, bool, error) {
	res := CompleteResult{State: StateReady, ID: u.modelID}

	a.mu.Lock()
	// 会话被作废了、存储在这期间换过了、或者模型被禁止了，都不能登记。
	sameStore := a.st.Load().gen == u.gen
	stale := a.isCancelled(u) || !sameStore
	blocked, err := a.db.IsBlocked(u.modelID)
	if err == nil && !stale && !blocked {
		err = a.db.PutModel(meta.Model{
			ID:         u.modelID,
			Name:       u.name,
			Size:       u.size,
			Uploader:   u.uploader,
			UploadedAt: a.now().Unix(),
		})
	}
	if err == nil && !stale && !blocked {
		// 记录写好的同一刻把会话摘掉，状态查询不会看到“既不在传也不存在”的空档。
		a.detachLocked(u)
		a.finished[u.id] = finished{result: res, expires: a.now().Add(finishedTTL)}
		a.mu.Unlock()
		return res, true, nil
	}
	a.mu.Unlock()

	// 登记不成：把刚放进去的对象撤回来。没有记录的对象不会被任何人下载到，但也不该留在那里占地方。
	// 存储没换的话要先确认这个 id 确实没有记录，免得删掉一个登记在案的文件。
	withdraw := !sameStore
	if sameStore {
		_, exists, derr := a.db.Model(u.modelID)
		withdraw = derr == nil && !exists
	}
	if withdraw {
		ctx, cancel := backgroundContext(time.Minute)
		if serr := u.store.Delete(ctx, u.modelID); serr != nil {
			log.Printf("warning: cannot remove unregistered object %s: %v", u.modelID, serr)
		}
		cancel()
	}
	if err != nil {
		return CompleteResult{}, true, internalError("register model", err)
	}
	return CompleteResult{}, true, ErrUploadGone
}

// digest 读完 r，返回内容的 SHA-256、开头的几个字节和总长度。
func digest(r io.Reader) (sum, head []byte, n int64, err error) {
	h := sha256.New()
	buf := make([]byte, 1<<20)
	head = make([]byte, 0, len(bundleMagic))
	for {
		k, rerr := r.Read(buf)
		if k > 0 {
			if need := len(bundleMagic) - len(head); need > 0 {
				head = append(head, buf[:min(k, need)]...)
			}
			h.Write(buf[:k])
			n += int64(k)
		}
		if rerr == io.EOF {
			return h.Sum(nil), head, n, nil
		}
		if rerr != nil {
			return nil, nil, n, rerr
		}
	}
}

// check 核对内容：大小、文件头、哈希。返回拒收原因；空串表示通过。
// withHash 为 false 时不核对哈希（只在配置里关掉了直连上传的哈希核对时）。
func check(u *upload, n int64, head, sum []byte, withHash bool) string {
	if n != u.size {
		return ReasonSizeMismatch
	}
	if !bytes.Equal(head, bundleMagic) {
		return ReasonNotABundle
	}
	if withHash {
		if len(sum) < 16 || hex.EncodeToString(sum[:16]) != u.modelID {
			return ReasonHashMismatch
		}
	}
	return ""
}

// Abort 作废一个上传会话；会话不存在也不算错。
func (a *App) Abort(uploadID string) {
	if u := a.lookup(uploadID); u != nil {
		a.drop(u, true)
	}
}

// SweepUploads 作废空闲超时的上传会话，并清掉过期的会话结果。
func (a *App) SweepUploads() {
	now := a.now()
	var idle []*upload
	a.mu.Lock()
	for _, u := range a.uploads {
		limit := a.opts.ProxyIdleTimeout
		if u.mode == settings.TransferDirect {
			limit = a.opts.DirectIdleTimeout
		}
		if now.Sub(time.Unix(0, u.lastActive.Load())) > limit {
			idle = append(idle, u)
		}
	}
	for id, f := range a.finished {
		if now.After(f.expires) {
			delete(a.finished, id)
		}
	}
	a.mu.Unlock()
	for _, u := range idle {
		// 正在收尾的会话不算空闲（complete 可能要跑很久）。
		if !u.markClosed(false) {
			continue
		}
		log.Printf("upload %s for model %s timed out", u.id, u.modelID)
		a.mu.Lock()
		a.detachLocked(u)
		a.mu.Unlock()
		a.free(u)
	}
}

// UploadInfo 是管理页“进行中的上传”里的一项。
type UploadInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Uploader  string `json:"uploader"`
	Size      int64  `json:"size"`
	Received  int64  `json:"received"`
	Mode      string `json:"mode"`
	StartedAt int64  `json:"started_at"`
}

// ActiveUploads 返回进行中的上传会话（按开始时间排序）。
func (a *App) ActiveUploads() []UploadInfo {
	a.mu.RLock()
	all := make([]*upload, 0, len(a.uploads))
	for _, u := range a.uploads {
		all = append(all, u)
	}
	a.mu.RUnlock()

	out := make([]UploadInfo, 0, len(all))
	for _, u := range all {
		out = append(out, UploadInfo{
			ID:        u.modelID,
			Name:      u.name,
			Uploader:  u.uploader,
			Size:      u.size,
			Received:  u.receivedBytes(),
			Mode:      u.mode,
			StartedAt: u.startedAt,
		})
	}
	sortUploads(out)
	return out
}

// activeUploadCount 返回进行中的上传会话数。
func (a *App) activeUploadCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.uploads)
}
