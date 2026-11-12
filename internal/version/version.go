// Package version 保存程序的名字和版本号。
package version

// Name 是 ping 接口里报告的服务名。
const Name = "vml-sync-server"

// API 是接口约定的版本（docs/API.md 的 v1）。
const API = 1

// Version 是程序版本。发布构建可以用
// -ldflags "-X vml-sync-server/internal/version.Version=1.2.3" 覆盖。
var Version = "0.2.0"
