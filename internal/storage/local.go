package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// tmpPrefix 是本存储自己产生的临时文件的名字前缀；这样的文件不是对象，启动时会被清掉。
const tmpPrefix = ".tmp-"

// Local 把模型文件存在本地磁盘的一个目录里，文件名就是模型 id。
type Local struct {
	dir string
}

// NewLocal 创建本地存储；目录不存在时会建出来。
func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create storage directory: %w", err)
	}
	return &Local{dir: dir}, nil
}

// Kind 实现 Store。
func (l *Local) Kind() string { return KindLocal }

// path 返回 id 对应的文件路径。调用前 id 必须已经校验过。
func (l *Local) path(id string) (string, error) {
	if !ValidModelID(id) {
		return "", errBadID
	}
	return filepath.Join(l.dir, id), nil
}

// PutFile 实现 Store：先把内容落盘，再用改名的方式原子地放到最终位置。
func (l *Local) PutFile(_ context.Context, id, src string, _ int64, _ []byte) error {
	dst, err := l.path(id)
	if err != nil {
		return err
	}
	// 记录会在这之后写进元数据库；先确保文件内容真的落盘了，免得断电后记录在、内容却是坏的。
	if err := syncFile(src); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		syncDir(l.dir)
		return nil
	}
	// 改名失败多半是因为临时目录和存储目录不在同一个文件系统上：改成复制到存储目录再改名。
	tmp, err := l.copyIn(src)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(l.dir)
	os.Remove(src)
	return nil
}

// copyIn 把 src 复制成存储目录里的一个临时文件，返回它的路径。
func (l *Local) copyIn(src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	tmp := filepath.Join(l.dir, tmpPrefix+hex.EncodeToString(suffix[:]))
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// syncDir 尽力把目录项的改动落盘（Windows 上打不开目录，失败就算了）。
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// Open 实现 Store。
func (l *Local) Open(_ context.Context, id string, offset, length int64) (io.ReadCloser, error) {
	p, err := l.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	if length < 0 {
		return f, nil
	}
	return &limitedFile{Reader: io.LimitReader(f, length), Closer: f}, nil
}

type limitedFile struct {
	io.Reader
	io.Closer
}

// Stat 实现 Store。
func (l *Local) Stat(_ context.Context, id string) (Object, error) {
	p, err := l.path(id)
	if err != nil {
		return Object{}, err
	}
	info, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Object{}, ErrNotExist
		}
		return Object{}, err
	}
	if !info.Mode().IsRegular() {
		return Object{}, ErrNotExist
	}
	return Object{ID: id, Size: info.Size(), ModTime: info.ModTime()}, nil
}

// Delete 实现 Store。
func (l *Local) Delete(_ context.Context, id string) error {
	p, err := l.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// List 实现 Store。
func (l *Local) List(_ context.Context) ([]Object, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, e := range entries {
		if !ValidModelID(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		out = append(out, Object{ID: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	return out, nil
}

// Probe 实现 Store：在存储目录里写一个临时文件、读回来、删掉。
func (l *Local) Probe(_ context.Context) error {
	want := make([]byte, 32)
	if _, err := rand.Read(want); err != nil {
		return err
	}
	p := filepath.Join(l.dir, tmpPrefix+"probe-"+hex.EncodeToString(want[:8]))
	if err := os.WriteFile(p, want, 0o644); err != nil {
		return fmt.Errorf("cannot write to the storage directory: %w", err)
	}
	got, err := os.ReadFile(p)
	if rerr := os.Remove(p); err == nil {
		err = rerr
	}
	if err != nil {
		return fmt.Errorf("cannot read back or delete a file in the storage directory: %w", err)
	}
	if !bytes.Equal(got, want) {
		return errors.New("the storage directory returned different content than was written")
	}
	return nil
}

// CleanupStale 实现 Store：删掉上次运行留下的临时文件。
func (l *Local) CleanupStale(_ context.Context) error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) && e.Type().IsRegular() {
			os.Remove(filepath.Join(l.dir, e.Name()))
		}
	}
	return nil
}
