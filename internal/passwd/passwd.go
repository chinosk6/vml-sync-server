// Package passwd 负责管理员密码：加盐的慢哈希（PBKDF2-HMAC-SHA256，只用标准库）和初始密码的生成。
package passwd

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DefaultIterations 是哈希的迭代次数。一次核对在普通服务器上大约要零点几秒：
// 对正常登录没有影响，对拿到数据文件之后离线猜密码的人是实打实的成本。
const DefaultIterations = 600_000

const (
	scheme  = "pbkdf2-sha256"
	saltLen = 16
	keyLen  = 32

	// MinLength、MaxLength 是管理员密码的长度范围（按字符算）。
	MinLength = 8
	MaxLength = 128

	// 核对时接受的迭代次数范围（防止被改坏的数据把程序拖死）。
	minIterations = 1
	maxIterations = 10_000_000
)

// Hash 返回密码的哈希，格式是 pbkdf2-sha256$<迭代次数>$<盐>$<结果>（后两项是 base64）。
func Hash(password string, iterations int) (string, error) {
	if iterations <= 0 {
		iterations = DefaultIterations
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", scheme, iterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// Verify 报告 password 是否和 Hash 生成的哈希对得上。比较是常数时间的。
func Verify(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != scheme {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < minIterations || iterations > maxIterations {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := enc.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ErrWeak 表示新密码不符合长度要求。
var ErrWeak = errors.New("password must be 8 to 128 characters long")

// Check 检查一个新密码是否符合要求。
func Check(password string) error {
	n := len([]rune(password))
	if n < MinLength || n > MaxLength {
		return ErrWeak
	}
	return nil
}

// 初始密码用的字符：去掉了容易看混的 0 O o 1 l I i。
const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// Generate 随机生成一个初始密码：20 个字符，每 5 个用短横线隔开，方便照着日志或文件抄。
func Generate() (string, error) {
	const groups, groupLen = 4, 5
	var b strings.Builder
	buf := make([]byte, 1)
	for g := 0; g < groups; g++ {
		if g > 0 {
			b.WriteByte('-')
		}
		for i := 0; i < groupLen; {
			if _, err := rand.Read(buf); err != nil {
				return "", err
			}
			// 丢掉会让分布不均匀的那一截。
			if int(buf[0]) >= 256-256%len(alphabet) {
				continue
			}
			b.WriteByte(alphabet[int(buf[0])%len(alphabet)])
			i++
		}
	}
	return b.String(), nil
}
