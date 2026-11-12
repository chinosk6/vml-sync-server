package meta

import (
	"path/filepath"
	"testing"
)

func open(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestModels(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "meta.db"))

	if _, ok, err := db.Model("a"); ok || err != nil {
		t.Fatalf("Model on empty db: %v %v", ok, err)
	}
	if _, ok, err := db.RecordDownload("a", 100); ok || err != nil {
		t.Fatalf("RecordDownload on a missing model: %v %v", ok, err)
	}
	if _, ok, err := db.DeleteModel("a"); ok || err != nil {
		t.Fatalf("DeleteModel on a missing model: %v %v", ok, err)
	}

	a := Model{ID: "a", Name: "桔梗.vrca", Size: 100, Uploader: "小明", UploadedAt: 10}
	b := Model{ID: "b", Name: "b.vrca", Size: 250, UploadedAt: 20}
	for _, m := range []Model{a, b} {
		if err := db.PutModel(m); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok, err := db.Model("a"); !ok || err != nil || got != a {
		t.Fatalf("Model = %+v %v %v", got, ok, err)
	}
	if count, total, err := db.Usage(); count != 2 || total != 350 || err != nil {
		t.Fatalf("Usage = %d %d %v", count, total, err)
	}

	// 下载：次数加一，最近下载时间更新；过期计时的起点随之后移。
	if a.LastUsed() != 10 {
		t.Fatalf("LastUsed before any download = %d", a.LastUsed())
	}
	for i, now := range []int64{50, 70} {
		got, ok, err := db.RecordDownload("a", now)
		if !ok || err != nil || got.DownloadCount != int64(i+1) || got.LastDownloadAt != now || got.LastUsed() != now {
			t.Fatalf("RecordDownload = %+v %v %v", got, ok, err)
		}
	}
	if got, _, _ := db.Model("a"); got.DownloadCount != 2 || got.LastDownloadAt != 70 || got.Name != a.Name {
		t.Fatalf("Model after downloads = %+v", got)
	}

	all, err := db.Models()
	if err != nil || len(all) != 2 || all[0].ID != "a" || all[1].ID != "b" {
		t.Fatalf("Models = %+v %v", all, err)
	}
	if got, ok, err := db.DeleteModel("a"); !ok || err != nil || got.ID != "a" {
		t.Fatalf("DeleteModel = %+v %v %v", got, ok, err)
	}
	if count, total, _ := db.Usage(); count != 1 || total != 250 {
		t.Fatalf("Usage after delete = %d %d", count, total)
	}
}

func TestBlocklist(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "meta.db"))

	if blocked, err := db.IsBlocked("a"); blocked || err != nil {
		t.Fatalf("IsBlocked on empty db: %v %v", blocked, err)
	}
	if added, err := db.Block(Blocked{ID: "a", Name: "a.vrca", Note: "first", BlockedAt: 100}); !added || err != nil {
		t.Fatalf("Block = %v %v", added, err)
	}
	// 再加一次：不算新增，保留原来的时间和名字，备注更新。
	if added, err := db.Block(Blocked{ID: "a", Note: "second", BlockedAt: 999}); added || err != nil {
		t.Fatalf("Block again = %v %v", added, err)
	}
	db.Block(Blocked{ID: "b", BlockedAt: 200})

	list, err := db.Blocklist()
	if err != nil || len(list) != 2 {
		t.Fatalf("Blocklist = %+v %v", list, err)
	}
	if list[0] != (Blocked{ID: "a", Name: "a.vrca", Note: "second", BlockedAt: 100}) {
		t.Fatalf("entry = %+v", list[0])
	}
	if n, _ := db.BlockedCount(); n != 2 {
		t.Fatalf("BlockedCount = %d", n)
	}
	if blocked, _ := db.IsBlocked("a"); !blocked {
		t.Fatal("a should be blocked")
	}

	if removed, err := db.Unblock([]string{"a", "zzz", "a"}); removed != 1 || err != nil {
		t.Fatalf("Unblock = %d %v", removed, err)
	}
	if blocked, _ := db.IsBlocked("a"); blocked {
		t.Fatal("a should not be blocked any more")
	}
	if n, _ := db.BlockedCount(); n != 1 {
		t.Fatalf("BlockedCount = %d", n)
	}
}
