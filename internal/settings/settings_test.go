package settings

import (
	"encoding/json"
	"strings"
	"testing"
)

func valid() Settings {
	s := Default()
	s.AuthKey = "a-long-random-client-key"
	return s
}

func validS3() Settings {
	s := valid()
	s.Storage.Type = StorageS3
	s.Storage.S3.Endpoint = "https://account.r2.cloudflarestorage.com"
	s.Storage.S3.Bucket = "models"
	s.Storage.S3.AccessKeyID = "ak"
	s.Storage.S3.SecretAccessKey = "sk"
	return s
}

// 默认值和文档 3.5 的表一致，并且（补上客户端密钥之后）是合法的。
func TestDefaults(t *testing.T) {
	s := Default()
	if s.RetentionHours != 72 || s.BasePath != "" || s.AuthKey != "" || s.MaxFileMB != 200 || s.MaxTotalMB != 0 ||
		s.ChunkMB != 5 || s.MaxActiveUploads != 8 || s.Transfer != "proxy" || s.DirectURLTTLSeconds != 900 ||
		!s.VerifyDirectUploads || s.TrustForwardedHeaders || s.Storage.Type != "local" ||
		s.Storage.S3.Region != "auto" || s.Storage.S3.Prefix != "models/" || !s.Storage.S3.PathStyle {
		t.Fatalf("defaults = %+v", s)
	}
	if err := s.Validate(); err == nil || err.Field != "auth_key" {
		t.Fatalf("defaults without a client key: %v", err)
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("defaults with a client key: %v", err)
	}
	if err := validS3().Validate(); err != nil {
		t.Fatalf("s3 settings: %v", err)
	}

	key, err := NewAuthKey()
	if err != nil || len(key) < MinAuthKeyLen || len(key) > MaxAuthKeyLen {
		t.Fatalf("NewAuthKey = %q, %v", key, err)
	}
	if other, _ := NewAuthKey(); other == key {
		t.Fatal("NewAuthKey returned the same key twice")
	}
	s.AuthKey = key
	if err := s.Validate(); err != nil {
		t.Fatalf("generated key is not valid: %v", err)
	}
}

// 取值范围的两端。
func TestValidateBounds(t *testing.T) {
	ok := []func(*Settings){
		func(s *Settings) { s.RetentionHours = 0 },
		func(s *Settings) { s.RetentionHours = 87600 },
		func(s *Settings) { s.BasePath = "/" + strings.Repeat("x", 63) },
		func(s *Settings) { s.BasePath = "/a/b-c_d.e~f/0" },
		func(s *Settings) { s.AuthKey = strings.Repeat("k", 16) },
		func(s *Settings) { s.AuthKey = strings.Repeat("k", 128) },
		func(s *Settings) { s.AuthKey = "!#$%&'()*+,-./:;<=>?@[]^_`{|}~" },
		func(s *Settings) { s.MaxFileMB = 1 },
		func(s *Settings) { s.MaxFileMB = 5000 },
		func(s *Settings) { s.MaxTotalMB = 1 << 30 },
		func(s *Settings) { s.ChunkMB = 1 },
		func(s *Settings) { s.ChunkMB = 64 },
		func(s *Settings) { s.MaxActiveUploads = 1 },
		func(s *Settings) { s.MaxActiveUploads = 64 },
		func(s *Settings) { s.DirectURLTTLSeconds = 60 },
		func(s *Settings) { s.DirectURLTTLSeconds = 86400 },
		// 没在用 S3 时，没填完的 S3 设置不算错。
		func(s *Settings) { s.Storage.S3.Bucket = "half-filled" },
		func(s *Settings) { s.Storage.S3.Endpoint = "http://minio.local:9000" },
	}
	for i, change := range ok {
		s := valid()
		change(&s)
		if err := s.Validate(); err != nil {
			t.Errorf("valid case %d rejected: %v", i, err)
		}
	}

	direct := validS3()
	direct.Transfer = TransferDirect
	if err := direct.Validate(); err != nil {
		t.Fatalf("direct with s3: %v", err)
	}
	direct.ChunkMB = 4
	if err := direct.Validate(); err == nil || err.Field != "chunk_mb" {
		t.Fatalf("direct with 4 MB chunks: %v", err)
	}
	local := valid()
	local.Transfer = TransferDirect
	if err := local.Validate(); err == nil || err.Field != "transfer" {
		t.Fatalf("direct with local storage: %v", err)
	}
}

// 局部更新：只改带上的；secret_access_key 不带或为空 = 保持原来的。
func TestPatch(t *testing.T) {
	base := validS3()

	var p Patch
	if err := json.Unmarshal([]byte(`{"max_file_mb": 50, "storage": {"s3": {"bucket": " other ", "secret_access_key": ""}}}`), &p); err != nil {
		t.Fatal(err)
	}
	got := p.Apply(base)
	want := base
	want.MaxFileMB = 50
	want.Storage.S3.Bucket = "other"
	if got != want {
		t.Fatalf("Apply = %+v\nwant   %+v", got, want)
	}
	if base.MaxFileMB != 200 || base.Storage.S3.Bucket != "models" {
		t.Fatal("Apply modified its argument")
	}
	if keys := strings.Join(p.Keys(), ","); keys != "max_file_mb,storage.s3.bucket" {
		t.Fatalf("Keys = %q", keys)
	}

	// 空的更新什么都不改。
	if got := (Patch{}).Apply(base); got != base {
		t.Fatalf("empty patch changed something: %+v", got)
	}
	if got := (*StoragePatch)(nil).Apply(base.Storage); got != base.Storage {
		t.Fatal("nil storage patch changed something")
	}

	// 带了新的 secret 才换；值会去掉首尾空白（从控制台复制时常带进来）。
	json.Unmarshal([]byte(`{"storage": {"s3": {"secret_access_key": "  new-secret\n", "endpoint": "https://host/", "region": ""}}}`), &p)
	got = p.Apply(base)
	if got.Storage.S3.SecretAccessKey != "new-secret" || got.Storage.S3.Endpoint != "https://host" || got.Storage.S3.Region != "auto" {
		t.Fatalf("Apply = %+v", got.Storage.S3)
	}

	// null 等于没带。
	var nulls Patch
	if err := json.Unmarshal([]byte(`{"max_file_mb": null, "base_path": null, "storage": null, "verify_direct_uploads": null}`), &nulls); err != nil {
		t.Fatal(err)
	}
	if got := nulls.Apply(base); got != base || len(nulls.Keys()) != 0 {
		t.Fatalf("null values changed something: %+v", got)
	}
}

// 对外输出的形式里没有 secret_access_key。
func TestViewHidesSecret(t *testing.T) {
	s := validS3()
	s.Storage.S3.SecretAccessKey = "super-secret-value"
	raw, err := json.Marshal(s.View())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret-value") || strings.Contains(string(raw), `"secret_access_key"`) {
		t.Fatalf("View leaks the secret: %s", raw)
	}
	if !strings.Contains(string(raw), `"secret_access_key_set":true`) {
		t.Fatalf("View = %s", raw)
	}
	s.Storage.S3.SecretAccessKey = ""
	raw, _ = json.Marshal(s.View())
	if !strings.Contains(string(raw), `"secret_access_key_set":false`) {
		t.Fatalf("View = %s", raw)
	}
	// 保存的形式和输出的形式，键是一样的（除了 secret 那一项）。
	var stored, viewed map[string]any
	json.Unmarshal(raw, &viewed)
	full, _ := json.Marshal(s)
	json.Unmarshal(full, &stored)
	for key := range stored {
		if _, ok := viewed[key]; !ok {
			t.Errorf("View is missing %q", key)
		}
	}
}

// 什么算“换存储”。
func TestSameLocation(t *testing.T) {
	a := validS3().Storage
	same := a
	same.S3.AccessKeyID, same.S3.SecretAccessKey = "other", "other"
	if !SameLocation(a, same) || SameCredentials(a, same) {
		t.Fatal("changing only the credentials must not count as a new location")
	}
	for name, change := range map[string]func(*Storage){
		"type":       func(s *Storage) { s.Type = StorageLocal },
		"endpoint":   func(s *Storage) { s.S3.Endpoint = "https://other" },
		"region":     func(s *Storage) { s.S3.Region = "eu" },
		"bucket":     func(s *Storage) { s.S3.Bucket = "other" },
		"prefix":     func(s *Storage) { s.S3.Prefix = "other/" },
		"path_style": func(s *Storage) { s.S3.PathStyle = false },
	} {
		b := a
		change(&b)
		if SameLocation(a, b) {
			t.Errorf("changing %s must count as a new location", name)
		}
	}
	// 用本地磁盘时，S3 那一栏怎么改都不算换存储。
	l1, l2 := valid().Storage, valid().Storage
	l2.S3.Bucket, l2.S3.Endpoint = "x", "https://y"
	if !SameLocation(l1, l2) {
		t.Fatal("editing unused s3 fields must not count as a new location")
	}
}
