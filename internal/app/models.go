package app

import (
	"context"
	"errors"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"vml-sync-server/internal/meta"
	"vml-sync-server/internal/settings"
	"vml-sync-server/internal/storage"
	"vml-sync-server/internal/version"
)

// Ping 是“探测”的响应（docs/API.md 2.1）。
type Ping struct {
	OK           bool   `json:"ok"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	API          int    `json:"api"`
	Transfer     string `json:"transfer"`
	MaxFileBytes int64  `json:"max_file_bytes"`
	ChunkBytes   int64  `json:"chunk_bytes"`
}

// Ping 返回探测信息。存储不可用时它照常回答：客户端据此知道地址和密钥是对的。
func (a *App) Ping() Ping {
	st := a.st.Load()
	return Ping{
		OK:           true,
		Name:         version.Name,
		Version:      version.Version,
		API:          version.API,
		Transfer:     st.set.Transfer,
		MaxFileBytes: st.maxFileBytes,
		ChunkBytes:   st.chunkBytes,
	}
}

// Status 是“查询模型状态”的响应（docs/API.md 2.2）。
type Status struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Size     *int64 `json:"size,omitempty"`
	Received *int64 `json:"received,omitempty"`
}

// Status 返回一个模型现在的状态。这个调用不算一次下载。
func (a *App) Status(id string) (Status, error) {
	if _, err := a.current(); err != nil {
		return Status{}, err
	}
	blocked, err := a.db.IsBlocked(id)
	if err != nil {
		return Status{}, internalError("model status", err)
	}
	if blocked {
		return Status{ID: id, State: StateBlocked}, nil
	}
	// 先看会话再看记录：完成上传时是“写记录、摘会话”在同一把锁里做的，
	// 这个顺序保证不会把刚传完的模型误报成 missing。
	a.mu.RLock()
	u := a.byModel[id]
	a.mu.RUnlock()
	m, exists, err := a.db.Model(id)
	if err != nil {
		return Status{}, internalError("model status", err)
	}
	if exists {
		return Status{ID: id, State: StateReady, Size: ptr(m.Size)}, nil
	}
	if u != nil {
		return Status{ID: id, State: StateUploading, Size: ptr(u.size), Received: ptr(u.receivedBytes())}, nil
	}
	return Status{ID: id, State: StateMissing}, nil
}

// DownloadResult 是“下载”的响应（docs/API.md 2.6）。
type DownloadResult struct {
	State     string `json:"state"`
	Mode      string `json:"mode,omitempty"`
	Size      *int64 `json:"size,omitempty"`
	URL       string `json:"url,omitempty"`
	ExpiresIn *int   `json:"expires_in,omitempty"`
}

// Download 登记一次下载并告诉客户端怎么取文件。forceProxy 表示客户端要求这一次走中转。
func (a *App) Download(id string, forceProxy bool) (DownloadResult, error) {
	st, err := a.current()
	if err != nil {
		return DownloadResult{}, err
	}
	blocked, err := a.db.IsBlocked(id)
	if err != nil {
		return DownloadResult{}, internalError("download", err)
	}
	if blocked {
		return DownloadResult{State: StateBlocked}, nil
	}

	a.mu.RLock()
	uploading := a.byModel[id] != nil
	a.mu.RUnlock()

	// 和过期清理排队：要么清理先删掉（这里看到 missing），要么这里先续上期（清理就不会删）。
	unlock := a.lockID(id)
	m, ok, err := a.db.RecordDownload(id, a.now().Unix())
	unlock()
	if err != nil {
		return DownloadResult{}, internalError("download", err)
	}
	if !ok {
		if uploading {
			return DownloadResult{State: StateUploading}, nil
		}
		return DownloadResult{State: StateMissing}, nil
	}

	if st.set.Transfer == settings.TransferDirect && st.direct != nil && !forceProxy {
		ttl := st.set.DirectURLTTLSeconds
		url, err := st.direct.DownloadURL(id, time.Duration(ttl)*time.Second)
		if err != nil {
			return DownloadResult{}, internalError("sign download url", err)
		}
		return DownloadResult{
			State:     StateReady,
			Mode:      settings.TransferDirect,
			Size:      ptr(m.Size),
			URL:       url,
			ExpiresIn: ptr(ttl),
		}, nil
	}
	return DownloadResult{State: StateReady, Mode: settings.TransferProxy, Size: ptr(m.Size)}, nil
}

// FileInfo 返回一个可以下载的模型的记录；不存在或者被禁止时返回 ErrNotFound。
func (a *App) FileInfo(id string) (meta.Model, error) {
	if _, err := a.current(); err != nil {
		return meta.Model{}, err
	}
	blocked, err := a.db.IsBlocked(id)
	if err != nil {
		return meta.Model{}, internalError("file info", err)
	}
	if blocked {
		return meta.Model{}, ErrNotFound
	}
	m, ok, err := a.db.Model(id)
	if err != nil {
		return meta.Model{}, internalError("file info", err)
	}
	if !ok {
		return meta.Model{}, ErrNotFound
	}
	return m, nil
}

// OpenFile 打开一个模型的内容，从 offset 开始读 length 个字节。
// 只有登记过（也就是通过了核对）的模型才打得开。
func (a *App) OpenFile(ctx context.Context, id string, offset, length int64) (io.ReadCloser, error) {
	st, err := a.current()
	if err != nil {
		return nil, err
	}
	if _, err := a.FileInfo(id); err != nil {
		return nil, err
	}
	rc, err := st.store.Open(ctx, id, offset, length)
	if err == nil {
		return rc, nil
	}
	if errors.Is(err, storage.ErrNotExist) {
		// 记录还在、文件却没了（比如被人从存储里手工删掉）：把记录也清掉，
		// 这样持有文件的玩家下次会重新上传。
		a.forgetDangling(ctx, id)
		return nil, ErrNotFound
	}
	return nil, internalError("open model file", err)
}

// forgetDangling 删掉一条“存储里已经没有对应对象”的记录。
func (a *App) forgetDangling(ctx context.Context, id string) {
	unlock := a.lockID(id)
	defer unlock()
	// 拿到锁之后按现在的存储再确认一遍（存储可能刚换过，文件可能刚被重新传上来）。
	st, err := a.current()
	if err != nil {
		return
	}
	if _, err := st.store.Stat(ctx, id); !errors.Is(err, storage.ErrNotExist) {
		return
	}
	if _, ok, err := a.db.DeleteModel(id); err == nil && ok {
		log.Printf("warning: model %s has a record but no object in storage; record removed", id)
	}
}

// Overview 是管理页的概况（docs/API.md 3.1）。
type Overview struct {
	Version         string `json:"version"`
	Storage         string `json:"storage"`
	Transfer        string `json:"transfer"`
	MaxFileBytes    int64  `json:"max_file_bytes"`
	MaxTotalBytes   int64  `json:"max_total_bytes"`
	ChunkBytes      int64  `json:"chunk_bytes"`
	ModelCount      int    `json:"model_count"`
	TotalBytes      int64  `json:"total_bytes"`
	BlockedCount    int    `json:"blocked_count"`
	ActiveUploads   int    `json:"active_uploads"`
	RetentionHours  int    `json:"retention_hours"`
	LastCleanupAt   int64  `json:"last_cleanup_at"`
	NextCleanupAt   int64  `json:"next_cleanup_at"`
	Now             int64  `json:"now"`
	InitialPassword bool   `json:"initial_password"`
	DiskFreeBytes   int64  `json:"disk_free_bytes"`
}

// Overview 返回概况。
func (a *App) Overview() (Overview, error) {
	st := a.st.Load()
	count, total, err := a.db.Usage()
	if err != nil {
		return Overview{}, internalError("overview", err)
	}
	blocked, err := a.db.BlockedCount()
	if err != nil {
		return Overview{}, internalError("overview", err)
	}
	return Overview{
		Version:         version.Version,
		Storage:         st.set.Storage.Type,
		Transfer:        st.set.Transfer,
		MaxFileBytes:    st.maxFileBytes,
		MaxTotalBytes:   st.maxTotalBytes,
		ChunkBytes:      st.chunkBytes,
		ModelCount:      count,
		TotalBytes:      total,
		BlockedCount:    blocked,
		ActiveUploads:   a.activeUploadCount(),
		RetentionHours:  st.set.RetentionHours,
		LastCleanupAt:   a.lastCleanup.Load(),
		NextCleanupAt:   a.nextCleanup.Load(),
		Now:             a.now().Unix(),
		InitialPassword: a.InitialPassword(),
		DiskFreeBytes:   diskFree(a.dataDir),
	}, nil
}

// ModelItem 是模型列表里的一项（docs/API.md 3.2）。
type ModelItem struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	Uploader       string `json:"uploader"`
	UploadedAt     int64  `json:"uploaded_at"`
	LastDownloadAt int64  `json:"last_download_at"`
	DownloadCount  int64  `json:"download_count"`
	ExpiresAt      int64  `json:"expires_at"`
}

// ListQuery 是模型列表的查询条件。
type ListQuery struct {
	Search string
	Sort   string
	Desc   bool
	Offset int
	Limit  int
}

// 模型列表允许的排序字段。
var modelSorts = map[string]func(a, b *ModelItem) int{
	"uploaded_at":      func(a, b *ModelItem) int { return cmpInt(a.UploadedAt, b.UploadedAt) },
	"last_download_at": func(a, b *ModelItem) int { return cmpInt(a.LastDownloadAt, b.LastDownloadAt) },
	"size":             func(a, b *ModelItem) int { return cmpInt(a.Size, b.Size) },
	"download_count":   func(a, b *ModelItem) int { return cmpInt(a.DownloadCount, b.DownloadCount) },
	"expires_at":       func(a, b *ModelItem) int { return cmpInt(a.ExpiresAt, b.ExpiresAt) },
	"name": func(a, b *ModelItem) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	},
	"uploader": func(a, b *ModelItem) int {
		return strings.Compare(strings.ToLower(a.Uploader), strings.ToLower(b.Uploader))
	},
}

// ValidModelSort 报告 s 是不是允许的排序字段。
func ValidModelSort(s string) bool {
	_, ok := modelSorts[s]
	return ok
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ListModels 返回符合条件的模型总数和其中一页。
func (a *App) ListModels(q ListQuery) (int, []ModelItem, error) {
	models, err := a.db.Models()
	if err != nil {
		return 0, nil, internalError("list models", err)
	}
	hours := a.st.Load().set.RetentionHours

	needle := strings.ToLower(q.Search)
	items := make([]ModelItem, 0, len(models))
	for _, m := range models {
		if needle != "" &&
			!strings.Contains(strings.ToLower(m.Name), needle) &&
			!strings.Contains(strings.ToLower(m.Uploader), needle) &&
			!strings.Contains(m.ID, needle) {
			continue
		}
		item := ModelItem{
			ID:             m.ID,
			Name:           m.Name,
			Size:           m.Size,
			Uploader:       m.Uploader,
			UploadedAt:     m.UploadedAt,
			LastDownloadAt: m.LastDownloadAt,
			DownloadCount:  m.DownloadCount,
		}
		if hours > 0 {
			item.ExpiresAt = m.LastUsed() + int64(hours)*3600
		}
		items = append(items, item)
	}

	cmp := modelSorts[q.Sort]
	if cmp == nil {
		cmp = modelSorts["uploaded_at"]
	}
	sort.SliceStable(items, func(i, j int) bool {
		c := cmp(&items[i], &items[j])
		if c == 0 {
			// 用 id 做第二排序键，翻页时顺序稳定。
			return items[i].ID < items[j].ID
		}
		if q.Desc {
			return c > 0
		}
		return c < 0
	})

	total := len(items)
	start := min(max(q.Offset, 0), total)
	end := min(start+max(q.Limit, 0), total)
	return total, items[start:end], nil
}

// deleteModelLocked 删除一个模型：先删存储里的对象，再删记录。调用方持有这个 id 的锁。
// 模型不存在时返回 false。
func (a *App) deleteModelLocked(ctx context.Context, store storage.Store, id string) (meta.Model, bool, error) {
	m, ok, err := a.db.Model(id)
	if err != nil || !ok {
		return m, false, err
	}
	if err := store.Delete(ctx, id); err != nil {
		return m, false, err
	}
	if _, _, err := a.db.DeleteModel(id); err != nil {
		return m, false, err
	}
	return m, true, nil
}

// blockLocked 把 id 加进禁止名单，并作废它正在进行的上传。调用方持有这个 id 的锁。
// name 为空时用现有记录里的文件名。
func (a *App) blockLocked(id, name, note string) (bool, error) {
	if name == "" {
		if m, ok, err := a.db.Model(id); err == nil && ok {
			name = m.Name
		}
	}
	a.mu.Lock()
	added, err := a.db.Block(meta.Blocked{ID: id, Name: name, Note: note, BlockedAt: a.now().Unix()})
	u := a.byModel[id]
	a.mu.Unlock()
	if err != nil {
		return false, err
	}
	if u != nil {
		a.drop(u, true)
	}
	return added, nil
}

// DeleteModels 删除一批模型；block 为 true 时同时把这些 id 加进禁止名单。
// 返回实际删掉的模型数和新加进名单的 id 数。
func (a *App) DeleteModels(ctx context.Context, ids []string, block bool, note string) (deleted, blocked int, err error) {
	if _, err := a.current(); err != nil {
		return 0, 0, err
	}
	note = CleanText(note, MaxNoteChars)
	seen := make(map[string]bool, len(ids))
	failed := 0
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true

		unlock := a.lockID(id)
		// 每个 id 拿到锁之后再取存储：存储可能在这一批删到一半时被换掉。
		st, serr := a.current()
		if serr != nil {
			unlock()
			return deleted, blocked, serr
		}
		if block {
			added, berr := a.blockLocked(id, "", note)
			if berr != nil {
				unlock()
				return deleted, blocked, internalError("block model", berr)
			}
			if added {
				blocked++
			}
		}
		_, ok, derr := a.deleteModelLocked(ctx, st.store, id)
		unlock()
		if derr != nil {
			log.Printf("error: delete model %s: %v", id, derr)
			failed++
			continue
		}
		if ok {
			deleted++
		}
	}
	if failed > 0 {
		return deleted, blocked, &Error{Status: 500, Code: "internal", Message: "some models could not be deleted from storage; see the server log"}
	}
	return deleted, blocked, nil
}

// BlockID 手动把一个 id 加进禁止名单；云端有这个文件的话一并删除。
func (a *App) BlockID(ctx context.Context, id, name, note string) error {
	name = CleanText(name, MaxNameChars)
	note = CleanText(note, MaxNoteChars)

	unlock := a.lockID(id)
	defer unlock()
	st, err := a.current()
	if err != nil {
		return err
	}
	if _, err := a.blockLocked(id, name, note); err != nil {
		return internalError("block model", err)
	}
	if _, _, err := a.deleteModelLocked(ctx, st.store, id); err != nil {
		return internalError("delete blocked model", err)
	}
	return nil
}

// Blocklist 返回禁止名单（新加的在前）。
func (a *App) Blocklist() ([]meta.Blocked, error) {
	items, err := a.db.Blocklist()
	if err != nil {
		return nil, internalError("blocklist", err)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].BlockedAt != items[j].BlockedAt {
			return items[i].BlockedAt > items[j].BlockedAt
		}
		return items[i].ID < items[j].ID
	})
	return items, nil
}

// Unblock 把这些 id 从禁止名单里去掉，返回实际去掉的个数。
func (a *App) Unblock(ids []string) (int, error) {
	n, err := a.db.Unblock(ids)
	if err != nil {
		return 0, internalError("unblock", err)
	}
	return n, nil
}

func sortUploads(items []UploadInfo) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].StartedAt != items[j].StartedAt {
			return items[i].StartedAt < items[j].StartedAt
		}
		return items[i].ID < items[j].ID
	})
}
