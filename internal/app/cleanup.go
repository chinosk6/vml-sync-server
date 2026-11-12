package app

import (
	"context"
	"encoding/hex"
	"log"
	"time"

	"vml-sync-server/internal/meta"
	"vml-sync-server/internal/storage"
)

// expired 报告一个模型是否已经连续 hours 小时没人下载。
func expired(m meta.Model, hours int, now int64) bool {
	return hours > 0 && now-m.LastUsed() > int64(hours)*3600
}

// Cleanup 跑一轮过期清理：删掉太久没人下载的模型（先删存储里的对象，再删记录）。
// 返回删掉的个数和腾出的字节数。存储不可用时返回 ErrStorageUnavailable。
func (a *App) Cleanup(ctx context.Context) (deleted int, freed int64, err error) {
	a.cleanMu.Lock()
	defer a.cleanMu.Unlock()

	st, err := a.current()
	if err != nil {
		return 0, 0, err
	}
	defer func() { a.lastCleanup.Store(a.now().Unix()) }()

	hours := st.set.RetentionHours
	if hours <= 0 {
		return 0, 0, nil
	}
	models, err := a.db.Models()
	if err != nil {
		return 0, 0, internalError("cleanup", err)
	}
	now := a.now().Unix()
	for _, m := range models {
		if !expired(m, hours, now) {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		unlock := a.lockID(m.ID)
		// 拿到锁之后再看一眼：可能刚好有人下载、续上了期；存储也可能刚被换掉。
		cur, ok, gerr := a.db.Model(m.ID)
		if now2 := a.st.Load(); gerr == nil && ok && now2.ready && now2.gen == st.gen && expired(cur, hours, now) {
			if _, removed, derr := a.deleteModelLocked(ctx, now2.store, m.ID); derr != nil {
				// 删不掉就留着记录，下一轮再试。
				log.Printf("error: cleanup: delete model %s: %v", m.ID, derr)
			} else if removed {
				deleted++
				freed += cur.Size
			}
		}
		unlock()
	}
	if deleted > 0 {
		log.Printf("cleanup: removed %d expired model(s), freed %d bytes", deleted, freed)
	}
	return deleted, freed, nil
}

// Run 跑后台任务，直到 ctx 结束：启动时清理一次，之后定时做过期清理和上传会话清理；
// 存储不可用时隔一阵重试一次。
func (a *App) Run(ctx context.Context) {
	cleanup := time.NewTicker(a.opts.CleanupInterval)
	defer cleanup.Stop()
	sweep := time.NewTicker(a.opts.SweepInterval)
	defer sweep.Stop()
	retry := time.NewTicker(a.opts.StorageRetryInterval)
	defer retry.Stop()

	a.nextCleanup.Store(a.now().Add(a.opts.CleanupInterval).Unix())
	a.Cleanup(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-cleanup.C:
			a.nextCleanup.Store(a.now().Add(a.opts.CleanupInterval).Unix())
			a.Cleanup(ctx)
		case <-sweep.C:
			a.SweepUploads()
		case <-retry.C:
			if ready, _ := a.StorageReady(); !ready {
				if err := a.Activate(ctx); err == nil {
					log.Printf("storage is usable again")
				}
			}
		}
	}
}

// reconcile 对账，让记录和 store 里的对象一致：
//   - 清掉 store 里上次留下的临时物；
//   - 有记录但存储里没有对象的，删掉记录；
//   - 存储里有对象但没有记录的，核对内容后补一条记录；
//   - 禁止名单里的 id 如果还有对象留着，删掉。
//
// 只在 store 还没对客户端开放的时候调用（启动时、换存储之后、存储从不可用恢复时）。
func (a *App) reconcile(ctx context.Context, store storage.Store) error {
	if err := store.CleanupStale(ctx); err != nil {
		return err
	}
	objects, err := store.List(ctx)
	if err != nil {
		return err
	}
	inStore := make(map[string]storage.Object, len(objects))
	for _, o := range objects {
		inStore[o.ID] = o
	}
	models, err := a.db.Models()
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(models))
	for _, m := range models {
		o, ok := inStore[m.ID]
		switch {
		case !ok:
			log.Printf("reconcile: model %s has a record but no object in storage; record removed", m.ID)
		case o.Size != m.Size:
			// 对象被人换过了：记录作废，下面把它当成没有记录的对象重新核对。
			log.Printf("reconcile: model %s has a different size in storage (%d, recorded %d); record removed", m.ID, o.Size, m.Size)
		default:
			known[m.ID] = true
			continue
		}
		if _, _, err := a.db.DeleteModel(m.ID); err != nil {
			return err
		}
	}

	for _, o := range objects {
		if known[o.ID] {
			continue
		}
		blocked, err := a.db.IsBlocked(o.ID)
		if err != nil {
			return err
		}
		if blocked {
			log.Printf("reconcile: object %s is on the blocklist; deleted", o.ID)
			if err := store.Delete(ctx, o.ID); err != nil {
				return err
			}
			continue
		}
		ok, err := verifyObject(ctx, store, o)
		if err != nil {
			return err
		}
		if !ok {
			// 内容和名字对不上的东西不登记（也就永远下载不到），但也不擅自删除。
			log.Printf("reconcile: object %s does not look like the model its name claims; ignored (remove it by hand)", o.ID)
			continue
		}
		uploadedAt := o.ModTime.Unix()
		if o.ModTime.IsZero() {
			uploadedAt = a.now().Unix()
		}
		if err := a.db.PutModel(meta.Model{ID: o.ID, Name: UnknownName, Size: o.Size, UploadedAt: uploadedAt}); err != nil {
			return err
		}
		log.Printf("reconcile: object %s had no record; record added", o.ID)
	}
	return nil
}

// verifyObject 核对一个没有记录的对象：是资源包，并且内容的哈希和名字（id）对得上。
// 没核对过的内容不能被下载到，这条规则对“存储里凭空多出来的对象”同样适用。
func verifyObject(ctx context.Context, store storage.Store, o storage.Object) (bool, error) {
	rc, err := store.Open(ctx, o.ID, 0, -1)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	sum, head, n, err := digest(rc)
	if err != nil {
		return false, err
	}
	return n == o.Size && string(head) == string(bundleMagic) && hex.EncodeToString(sum[:16]) == o.ID, nil
}
