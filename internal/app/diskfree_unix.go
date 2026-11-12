//go:build linux || darwin || freebsd

package app

import "syscall"

// diskFree 返回 path 所在磁盘上普通用户还能用的字节数；拿不到时返回 0。
func diskFree(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	// 各平台上这两个字段的类型不一样，统一转成 int64 再乘。
	free := int64(st.Bavail) * int64(st.Bsize)
	if free < 0 {
		return 0
	}
	return free
}
