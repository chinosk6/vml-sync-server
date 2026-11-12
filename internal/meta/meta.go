// Package meta 保存元数据：模型记录、禁止名单、在管理页里修改的设置、管理员密码的哈希。
// 底层是 bbolt（纯 Go 的单文件嵌入式存储），文件放在数据目录里。
package meta

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketModels   = []byte("models")
	bucketBlocked  = []byte("blocklist")
	bucketSettings = []byte("settings")

	// settings 桶里的键。
	keySettings      = []byte("settings")        // 全部设置（JSON，格式由 internal/settings 决定）
	keyAdminPassword = []byte("admin_password")  // 管理员密码的哈希
	keyRetention     = []byte("retention_hours") // 0.1.0 留下的旧键，只在迁移时读一次
)

// Model 是一个已经通过核对、可以下载的模型的记录。
type Model struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	Uploader       string `json:"uploader"`
	UploadedAt     int64  `json:"uploaded_at"`
	LastDownloadAt int64  `json:"last_download_at"`
	DownloadCount  int64  `json:"download_count"`
}

// LastUsed 返回过期计时的起点：上传时间和最近下载时间里较晚的那个。
func (m Model) LastUsed() int64 {
	if m.LastDownloadAt > m.UploadedAt {
		return m.LastDownloadAt
	}
	return m.UploadedAt
}

// Blocked 是禁止名单里的一项。
type Blocked struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Note      string `json:"note"`
	BlockedAt int64  `json:"blocked_at"`
}

// DB 是元数据库。所有方法都可以并发调用。
type DB struct {
	bolt *bolt.DB
}

// Open 打开（不存在则创建）元数据库。
func Open(path string) (*DB, error) {
	// 同一个文件被另一个进程占着时，等几秒就报错，而不是一直卡住。
	b, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			return nil, fmt.Errorf("metadata file %s is locked: is another instance running with the same data directory?", path)
		}
		return nil, fmt.Errorf("cannot open metadata file %s: %w", path, err)
	}
	err = b.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketModels, bucketBlocked, bucketSettings} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Close()
		return nil, err
	}
	return &DB{bolt: b}, nil
}

// Close 关闭数据库。
func (d *DB) Close() error { return d.bolt.Close() }

// Model 按 id 取一条模型记录。
func (d *DB) Model(id string) (m Model, ok bool, err error) {
	err = d.bolt.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketModels).Get([]byte(id))
		if raw == nil {
			return nil
		}
		ok = true
		return json.Unmarshal(raw, &m)
	})
	return m, ok && err == nil, err
}

// PutModel 写入（或覆盖）一条模型记录。
func (d *DB) PutModel(m Model) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return d.bolt.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketModels).Put([]byte(m.ID), raw)
	})
}

// DeleteModel 删除一条模型记录，返回被删掉的那条（不存在时 ok 为 false）。
func (d *DB) DeleteModel(id string) (m Model, ok bool, err error) {
	err = d.bolt.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketModels)
		raw := b.Get([]byte(id))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			// 记录坏了也照样删掉。
			m = Model{ID: id}
		}
		ok = true
		return b.Delete([]byte(id))
	})
	return m, ok && err == nil, err
}

// Models 返回全部模型记录（按 id 排序）。
func (d *DB) Models() ([]Model, error) {
	var out []Model
	err := d.bolt.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketModels).ForEach(func(_, raw []byte) error {
			var m Model
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			out = append(out, m)
			return nil
		})
	})
	return out, err
}

// Usage 返回模型数量和它们的总大小。
func (d *DB) Usage() (count int, total int64, err error) {
	err = d.bolt.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketModels).ForEach(func(_, raw []byte) error {
			var m struct {
				Size int64 `json:"size"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			count++
			total += m.Size
			return nil
		})
	})
	return count, total, err
}

// RecordDownload 给一个模型记一次下载：次数加一，最近下载时间改成 now。
// 模型不存在时 ok 为 false，什么也不改。
func (d *DB) RecordDownload(id string, now int64) (m Model, ok bool, err error) {
	err = d.bolt.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketModels)
		raw := b.Get([]byte(id))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		m.DownloadCount++
		m.LastDownloadAt = now
		out, err := json.Marshal(m)
		if err != nil {
			return err
		}
		ok = true
		return b.Put([]byte(id), out)
	})
	return m, ok && err == nil, err
}

// IsBlocked 报告一个 id 是否在禁止名单里。
func (d *DB) IsBlocked(id string) (blocked bool, err error) {
	err = d.bolt.View(func(tx *bolt.Tx) error {
		blocked = tx.Bucket(bucketBlocked).Get([]byte(id)) != nil
		return nil
	})
	return blocked, err
}

// Block 把一个 id 加进禁止名单。已经在名单里时只更新备注（和空着的文件名），
// 保留原来的加入时间，added 为 false。
func (d *DB) Block(entry Blocked) (added bool, err error) {
	err = d.bolt.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketBlocked)
		if raw := b.Get([]byte(entry.ID)); raw != nil {
			var old Blocked
			if json.Unmarshal(raw, &old) == nil {
				entry.BlockedAt = old.BlockedAt
				if entry.Name == "" {
					entry.Name = old.Name
				}
				if entry.Note == "" {
					entry.Note = old.Note
				}
			}
		} else {
			added = true
		}
		out, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		return b.Put([]byte(entry.ID), out)
	})
	return added && err == nil, err
}

// Unblock 把这些 id 从禁止名单里去掉，返回实际去掉的个数。
func (d *DB) Unblock(ids []string) (removed int, err error) {
	err = d.bolt.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketBlocked)
		for _, id := range ids {
			if b.Get([]byte(id)) == nil {
				continue
			}
			if err := b.Delete([]byte(id)); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// Blocklist 返回整个禁止名单（按 id 排序）。
func (d *DB) Blocklist() ([]Blocked, error) {
	var out []Blocked
	err := d.bolt.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketBlocked).ForEach(func(_, raw []byte) error {
			var e Blocked
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			out = append(out, e)
			return nil
		})
	})
	return out, err
}

// BlockedCount 返回禁止名单的长度。
func (d *DB) BlockedCount() (n int, err error) {
	err = d.bolt.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bucketBlocked).Stats().KeyN
		return nil
	})
	return n, err
}

// Settings 返回保存的设置（原始 JSON）。还没保存过时 ok 为 false。
func (d *DB) Settings() (blob []byte, ok bool, err error) {
	err = d.bolt.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket(bucketSettings).Get(keySettings); raw != nil {
			blob = append([]byte(nil), raw...)
			ok = true
		}
		return nil
	})
	return blob, ok && err == nil, err
}

// SaveSettings 保存设置。clearModels 为 true 时在同一个事务里删掉全部模型记录
// （换存储位置时用：旧位置上的模型在新位置拿不到），并返回被删掉的记录。禁止名单不动。
func (d *DB) SaveSettings(blob []byte, clearModels bool) (removed []Model, err error) {
	err = d.bolt.Update(func(tx *bolt.Tx) error {
		if clearModels {
			err := tx.Bucket(bucketModels).ForEach(func(k, raw []byte) error {
				var m Model
				if json.Unmarshal(raw, &m) != nil {
					m = Model{ID: string(k)}
				}
				removed = append(removed, m)
				return nil
			})
			if err != nil {
				return err
			}
			if err := tx.DeleteBucket(bucketModels); err != nil {
				return err
			}
			if _, err := tx.CreateBucket(bucketModels); err != nil {
				return err
			}
		}
		return tx.Bucket(bucketSettings).Put(keySettings, blob)
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// AdminPassword 是管理员密码的保存形式：只有哈希，没有明文。
type AdminPassword struct {
	Hash string `json:"hash"`
	// Initial 表示这还是程序生成的初始密码（首次启动或 -reset-admin-password），管理员还没改过。
	Initial bool `json:"initial"`
}

// AdminPassword 返回保存的管理员密码。还没有时 ok 为 false。
func (d *DB) AdminPassword() (p AdminPassword, ok bool, err error) {
	err = d.bolt.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketSettings).Get(keyAdminPassword)
		if raw == nil {
			return nil
		}
		ok = true
		return json.Unmarshal(raw, &p)
	})
	return p, ok && err == nil, err
}

// SetAdminPassword 保存管理员密码。
func (d *DB) SetAdminPassword(p AdminPassword) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return d.bolt.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSettings).Put(keyAdminPassword, raw)
	})
}

// LegacyRetentionHours 返回 0.1.0 单独保存的 retention_hours（那时其余设置都在配置文件里）。
// 没有或者值不对时 ok 为 false。
func (d *DB) LegacyRetentionHours() (hours int, ok bool) {
	d.bolt.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketSettings).Get(keyRetention)
		if raw == nil {
			return nil
		}
		if v, err := strconv.Atoi(string(raw)); err == nil && v >= 0 {
			hours, ok = v, true
		}
		return nil
	})
	return hours, ok
}
