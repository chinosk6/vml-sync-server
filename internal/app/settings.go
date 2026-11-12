package app

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vml-sync-server/internal/meta"
	"vml-sync-server/internal/passwd"
	"vml-sync-server/internal/s3"
	"vml-sync-server/internal/settings"
	"vml-sync-server/internal/storage"
)

// probeTimeout 是验证一个存储能不能用最多花多久。
const probeTimeout = 30 * time.Second

// purgeTimeout 是换存储时清空旧存储最多花多久（尽力而为，超时就算了）。
const purgeTimeout = 5 * time.Minute

// loadOrInitSettings 读出保存的设置；还没有时（首次启动）生成默认设置和客户端密钥并保存。
func loadOrInitSettings(db *meta.DB) (settings.Settings, error) {
	blob, ok, err := db.Settings()
	if err != nil {
		return settings.Settings{}, err
	}
	set := settings.Default()
	if ok {
		// 解到默认值上：以后新增的设置项在旧数据里没有，自然取默认值。
		if err := json.Unmarshal(blob, &set); err != nil {
			return settings.Settings{}, fmt.Errorf("the saved settings cannot be read: %w", err)
		}
		if set.AuthKey != "" {
			return set, nil
		}
	} else if hours, ok := db.LegacyRetentionHours(); ok && hours <= settings.MaxRetentionHours {
		// 0.1.0 的数据目录：其余设置那时在配置文件里，只有这一项存在数据里，原样带过来。
		set.RetentionHours = hours
	}
	key, err := settings.NewAuthKey()
	if err != nil {
		return settings.Settings{}, err
	}
	set.AuthKey = key
	blob, err = json.Marshal(set)
	if err != nil {
		return settings.Settings{}, err
	}
	if _, err := db.SaveSettings(blob, false); err != nil {
		return settings.Settings{}, err
	}
	return set, nil
}

// ensureAdminPassword 在还没有管理员密码时生成初始密码：哈希存进数据库，明文写进初始密码文件。
// 返回新生成的明文；已经有密码时返回空串。
func ensureAdminPassword(db *meta.DB, dataDir string, iterations int) (string, error) {
	if _, ok, err := db.AdminPassword(); err != nil || ok {
		return "", err
	}
	return newInitialPassword(db, dataDir, iterations)
}

// newInitialPassword 生成一个新的初始密码并保存。
func newInitialPassword(db *meta.DB, dataDir string, iterations int) (string, error) {
	password, err := passwd.Generate()
	if err != nil {
		return "", err
	}
	hash, err := passwd.Hash(password, iterations)
	if err != nil {
		return "", err
	}
	// 先写文件再存哈希：宁可留下一个没用上的文件，也不要出现“密码生效了却没处可查”。
	path := InitialPasswordPath(dataDir)
	os.Remove(path)
	if err := os.WriteFile(path, []byte(password+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("cannot write the initial password file: %w", err)
	}
	os.Chmod(path, 0o600)
	if err := db.SetAdminPassword(meta.AdminPassword{Hash: hash, Initial: true}); err != nil {
		return "", err
	}
	return password, nil
}

// InitialPasswordPath 返回初始密码文件的路径。
func InitialPasswordPath(dataDir string) string {
	return filepath.Join(dataDir, InitialPasswordFile)
}

// ResetAdminPassword 生成一个新的管理员密码（-reset-admin-password）：写进初始密码文件并返回明文。
// 数据目录里还没有设置时顺便做首次启动的初始化。服务必须先停掉。
func ResetAdminPassword(db *meta.DB, dataDir string) (string, error) {
	if _, err := loadOrInitSettings(db); err != nil {
		return "", err
	}
	return newInitialPassword(db, dataDir, passwd.DefaultIterations)
}

// VerifyAdminPassword 报告 password 是不是管理员密码。
func (a *App) VerifyAdminPassword(password string) bool {
	// 超长的输入不可能是对的，不值得为它算一次慢哈希。
	if password == "" || len(password) > 4*passwd.MaxLength {
		return false
	}
	saved, ok, err := a.db.AdminPassword()
	if err != nil || !ok {
		return false
	}
	a.hashSlots <- struct{}{}
	defer func() { <-a.hashSlots }()
	return passwd.Verify(saved.Hash, password)
}

// InitialPassword 报告管理员是不是还在用程序生成的初始密码。
func (a *App) InitialPassword() bool {
	saved, ok, err := a.db.AdminPassword()
	return err == nil && ok && saved.Initial
}

// ChangeAdminPassword 把管理员密码改成 newPassword，并删掉初始密码文件。
// 调用方负责先核对当前密码。
func (a *App) ChangeAdminPassword(newPassword string) error {
	if err := passwd.Check(newPassword); err != nil {
		return &Error{Status: 400, Code: "weak_password", Message: err.Error()}
	}
	a.hashSlots <- struct{}{}
	hash, err := passwd.Hash(newPassword, a.opts.PasswordIterations)
	<-a.hashSlots
	if err != nil {
		return internalError("hash password", err)
	}
	if err := a.db.SetAdminPassword(meta.AdminPassword{Hash: hash}); err != nil {
		return internalError("save password", err)
	}
	if err := os.Remove(InitialPasswordPath(a.dataDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: cannot remove the initial password file: %v", err)
	}
	log.Printf("admin password changed")
	return nil
}

// Activate 让当前设置里的存储进入可用状态：对账成功之后才对客户端开放。
// 存储不可用时返回错误，程序照常运行，后台会隔一阵再试。
func (a *App) Activate(ctx context.Context) error {
	a.setMu.Lock()
	defer a.setMu.Unlock()
	return a.activateLocked(ctx)
}

// activateLocked 是 Activate 的实现。调用方持有 setMu。
func (a *App) activateLocked(ctx context.Context) error {
	st := a.st.Load()
	if st.ready {
		return nil
	}
	store, reason := st.store, ""
	if store == nil {
		if store, reason = a.openStore(st.set.Storage); store == nil {
			a.replaceStore(nil, false, reason)
			return errors.New(reason)
		}
	}
	// 这时候客户端的接口都是 503，没有上传会话，对账可以放心地改记录。
	if err := a.reconcile(ctx, store); err != nil {
		reason = describeStorageError(err)
		a.replaceStore(store, false, reason)
		return errors.New(reason)
	}
	a.replaceStore(store, true, "")
	return nil
}

// replaceStore 换上一个只有存储相关字段不同的新 state。调用方持有 setMu。
func (a *App) replaceStore(store storage.Store, ready bool, reason string) {
	next := *a.st.Load()
	next.setStore(store, reason)
	next.ready = ready
	a.st.Store(&next)
}

// UpdateSettings 套用一次局部更新：整体校验，全部合法才生效，立刻生效。
//
// 换存储位置（见 settings.SameLocation）等于清空，流程严格按 docs/API.md 3.5：
// 没带确认就拒绝；先验证新存储能用；然后作废进行中的上传、删掉旧存储里的模型对象和全部模型记录
// （禁止名单保留）；最后切到新存储。只换凭据不算换存储：验证能用之后直接生效。
func (a *App) UpdateSettings(ctx context.Context, p settings.Patch) (settings.Settings, error) {
	a.setMu.Lock()
	defer a.setMu.Unlock()

	cur := a.st.Load()
	next := p.Apply(cur.set)
	if ferr := next.Validate(); ferr != nil {
		return settings.Settings{}, &Error{Status: 400, Code: "invalid_settings", Message: ferr.Error(), Field: ferr.Field}
	}

	moved := !settings.SameLocation(cur.set.Storage, next.Storage)
	rekeyed := !moved && next.Storage.Type == settings.StorageS3 && !settings.SameCredentials(cur.set.Storage, next.Storage)

	var store storage.Store
	if moved || rekeyed {
		if moved && !p.ConfirmStorageChange {
			count, _, err := a.db.Usage()
			if err != nil {
				return settings.Settings{}, internalError("update settings", err)
			}
			return settings.Settings{}, &Error{
				Status:     409,
				Code:       "storage_change_needs_confirm",
				Message:    "changing the storage location discards every model stored so far; send confirm_storage_change: true to go ahead",
				ModelCount: &count,
			}
		}
		var reason string
		if store, reason = a.openStore(next.Storage); store == nil {
			return settings.Settings{}, &Error{Status: 400, Code: "storage_unreachable", Message: reason}
		}
		if err := probe(ctx, store); err != nil {
			return settings.Settings{}, &Error{Status: 400, Code: "storage_unreachable", Message: describeStorageError(err)}
		}
	}

	blob, err := json.Marshal(next)
	if err != nil {
		return settings.Settings{}, internalError("update settings", err)
	}

	if moved {
		// 从这里开始不再受请求取消的影响：要么全做完，要么（数据库写不进去）什么都没做。
		a.mu.Lock()
		removed, err := a.db.SaveSettings(blob, true)
		if err != nil {
			a.mu.Unlock()
			return settings.Settings{}, internalError("update settings", err)
		}
		var dropped []*upload
		for _, u := range a.uploads {
			// 正在收尾的会话留给收尾的一方善后：它登记时会发现存储已经换了。
			if u.markClosed(true) {
				a.detachLocked(u)
				dropped = append(dropped, u)
			}
		}
		n := a.newState(next, cur.gen+1)
		n.setStore(store, "")
		a.st.Store(n)
		a.mu.Unlock()

		for _, u := range dropped {
			a.free(u)
		}
		a.purge(cur.store, removed)
		log.Printf("storage location changed: %d model record(s) discarded, %d upload(s) cancelled", len(removed), len(dropped))
	} else {
		if _, err := a.db.SaveSettings(blob, false); err != nil {
			return settings.Settings{}, internalError("update settings", err)
		}
		n := a.newState(next, cur.gen)
		n.setStore(cur.store, cur.reason)
		n.ready = cur.ready
		if rekeyed {
			// 位置没变，记录仍然有效；之后的操作用新凭据。
			n.setStore(store, "")
		}
		a.st.Store(n)
	}
	log.Printf("settings updated: %s", strings.Join(p.Keys(), ", "))

	// 存储还没启用（刚换了位置，或者之前因为设置不对一直不可用）：现在就试一次。
	// 不成也不算这次修改失败：设置已经生效，后台会接着试。
	if err := a.activateLocked(ctx); err != nil {
		log.Printf("warning: storage is not usable yet: %v", err)
	}
	return next, nil
}

// purge 删掉旧存储里的模型对象。尽力而为：删不掉的只记日志。
func (a *App) purge(old storage.Store, models []meta.Model) {
	if old == nil || len(models) == 0 {
		return
	}
	ctx, cancel := backgroundContext(purgeTimeout)
	defer cancel()
	var (
		wg     sync.WaitGroup
		slots  = make(chan struct{}, 8)
		mu     sync.Mutex
		failed int
	)
	for _, m := range models {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			if err := old.Delete(ctx, m.ID); err != nil {
				mu.Lock()
				failed++
				if failed <= 5 {
					log.Printf("warning: cannot delete model %s from the old storage: %v", m.ID, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if failed > 0 {
		log.Printf("warning: %d of %d object(s) could not be deleted from the old storage", failed, len(models))
	}
}

// TestStorage 验证一组存储设置能不能用（写一个探测对象、读回来、删掉），不保存任何东西。
// patch 套在当前的存储设置上，所以 secret_access_key 不带或为空 = 用已保存的那个。
func (a *App) TestStorage(ctx context.Context, patch *settings.StoragePatch) (bool, string) {
	candidate := patch.Apply(a.Settings().Storage)
	if ferr := candidate.Validate(); ferr != nil {
		return false, ferr.Error()
	}
	store, reason := a.openStore(candidate)
	if store == nil {
		return false, reason
	}
	if err := probe(ctx, store); err != nil {
		return false, describeStorageError(err)
	}
	return true, ""
}

func probe(ctx context.Context, store storage.Store) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return store.Probe(ctx)
}

// describeStorageError 把存储操作的错误说成人话：让人看得出是地址不通、凭据不对还是桶不存在。
func describeStorageError(err error) string {
	var se *s3.Error
	if errors.As(err, &se) {
		switch {
		case se.Code == "NoSuchBucket":
			return "the bucket does not exist (NoSuchBucket): check storage.s3.bucket"
		case se.Code == "InvalidAccessKeyId" || se.Code == "SignatureDoesNotMatch" || se.Code == "AccessDenied" ||
			se.Code == "InvalidToken" || se.Code == "Unauthorized" || se.Status == 401 || se.Status == 403:
			code := se.Code
			if code == "" {
				code = fmt.Sprintf("HTTP %d", se.Status)
			}
			return "the storage rejected the credentials (" + code + "): check access_key_id and secret_access_key, and that the key may read and write this bucket"
		case se.Code != "":
			return fmt.Sprintf("the storage answered with an error: %s (HTTP %d): %s", se.Code, se.Status, se.Message)
		default:
			return fmt.Sprintf("the storage answered with HTTP %d: is storage.s3.endpoint really an S3-compatible service?", se.Status)
		}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "cannot reach the storage: the host name in storage.s3.endpoint cannot be resolved (" + dnsErr.Name + ")"
	}
	var certErr *x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &certErr) || errors.As(err, &hostErr) {
		return "cannot reach the storage: the endpoint's TLS certificate is not trusted: " + err.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "cannot reach the storage: timed out"
	}
	var netErr net.Error
	var opErr *net.OpError
	if errors.As(err, &opErr) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "cannot reach the storage (network error): " + err.Error()
	}
	return err.Error()
}
