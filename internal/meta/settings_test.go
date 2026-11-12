package meta

import (
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// 设置、管理员密码、记录都存在文件里，重新打开之后还在。
func TestSettingsPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// 全新的数据文件里什么都没有。
	if _, ok, err := db.Settings(); ok || err != nil {
		t.Fatalf("Settings on a new file: %v %v", ok, err)
	}
	if _, ok, err := db.AdminPassword(); ok || err != nil {
		t.Fatalf("AdminPassword on a new file: %v %v", ok, err)
	}
	if _, ok := db.LegacyRetentionHours(); ok {
		t.Fatal("LegacyRetentionHours on a new file")
	}

	if _, err := db.SaveSettings([]byte(`{"retention_hours":5}`), false); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAdminPassword(AdminPassword{Hash: "hash-1", Initial: true}); err != nil {
		t.Fatal(err)
	}
	db.PutModel(Model{ID: "a", Size: 5})
	db.PutModel(Model{ID: "b", Size: 7})
	db.Block(Blocked{ID: "x", Note: "keep me"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = open(t, path)
	if blob, ok, err := db.Settings(); !ok || err != nil || string(blob) != `{"retention_hours":5}` {
		t.Fatalf("Settings after reopening = %q %v %v", blob, ok, err)
	}
	if p, ok, err := db.AdminPassword(); !ok || err != nil || p != (AdminPassword{Hash: "hash-1", Initial: true}) {
		t.Fatalf("AdminPassword after reopening = %+v %v %v", p, ok, err)
	}
	if count, total, _ := db.Usage(); count != 2 || total != 12 {
		t.Fatalf("Usage after reopening = %d %d", count, total)
	}

	// 换存储位置时：保存新设置和清空模型记录是同一个事务；禁止名单和管理员密码不动。
	removed, err := db.SaveSettings([]byte(`{"retention_hours":9}`), true)
	if err != nil || len(removed) != 2 || removed[0].ID != "a" || removed[1].ID != "b" || removed[1].Size != 7 {
		t.Fatalf("SaveSettings(clear) = %+v %v", removed, err)
	}
	if count, total, _ := db.Usage(); count != 0 || total != 0 {
		t.Fatalf("Usage after clearing = %d %d", count, total)
	}
	if blob, _, _ := db.Settings(); string(blob) != `{"retention_hours":9}` {
		t.Fatalf("Settings after clearing = %q", blob)
	}
	if blocked, _ := db.IsBlocked("x"); !blocked {
		t.Fatal("blocklist was cleared together with the models")
	}
	if p, _, _ := db.AdminPassword(); p.Hash != "hash-1" {
		t.Fatalf("AdminPassword after clearing = %+v", p)
	}
	// 清空之后照常能写新记录。
	if err := db.PutModel(Model{ID: "c", Size: 1}); err != nil {
		t.Fatal(err)
	}
	if removed, err := db.SaveSettings([]byte(`{}`), true); err != nil || len(removed) != 1 {
		t.Fatalf("second clear = %+v %v", removed, err)
	}
	if removed, err := db.SaveSettings([]byte(`{}`), true); err != nil || len(removed) != 0 {
		t.Fatalf("clearing an empty db = %+v %v", removed, err)
	}

	if err := db.SetAdminPassword(AdminPassword{Hash: "hash-2"}); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := db.AdminPassword(); p != (AdminPassword{Hash: "hash-2"}) {
		t.Fatalf("AdminPassword after change = %+v", p)
	}
}

// 0.1.0 把 retention_hours 单独存在一个键里。
func TestLegacyRetentionHours(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "meta.db"))
	put := func(value string) {
		t.Helper()
		err := db.bolt.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(bucketSettings).Put(keyRetention, []byte(value))
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	put("24")
	if hours, ok := db.LegacyRetentionHours(); !ok || hours != 24 {
		t.Fatalf("LegacyRetentionHours = %d %v", hours, ok)
	}
	put("0")
	if hours, ok := db.LegacyRetentionHours(); !ok || hours != 0 {
		t.Fatalf("LegacyRetentionHours = %d %v", hours, ok)
	}
	for _, bad := range []string{"", "abc", "-5"} {
		put(bad)
		if _, ok := db.LegacyRetentionHours(); ok {
			t.Fatalf("LegacyRetentionHours accepted %q", bad)
		}
	}
}
