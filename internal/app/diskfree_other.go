//go:build !linux && !darwin && !freebsd && !windows

package app

// diskFree 在这个平台上拿不到磁盘剩余空间，按约定返回 0。
func diskFree(string) int64 { return 0 }
