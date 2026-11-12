// Package s3 是一个只包含本项目用得到的操作的 S3 兼容客户端（AWS Signature V4）。
// 自己实现而不引入官方 SDK，是为了少带依赖，并且能完全控制预签名地址里签了哪些东西。
package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	algorithm = "AWS4-HMAC-SHA256"
	service   = "s3"

	// unsignedPayload 用在预签名地址里：签名不覆盖请求体。
	unsignedPayload = "UNSIGNED-PAYLOAD"
	// emptyPayloadHash 是空请求体的 SHA-256。
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	timeFormat = "20060102T150405Z"
	dateFormat = "20060102"
)

// signer 持有计算签名需要的凭据。
type signer struct {
	accessKey string
	secretKey string
	region    string
}

// uriEncode 按 SigV4 的规则做百分号编码：只有 A-Z a-z 0-9 - _ . ~ 原样保留。
// encodeSlash 为 false 时 '/' 也保留（用于对象名里的路径分隔）。
func uriEncode(s string, encodeSlash bool) string {
	const hexChars = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexChars[c>>4])
			b.WriteByte(hexChars[c&15])
		}
	}
	return b.String()
}

// canonicalQuery 把查询参数编码并排序，得到既用于签名、也原样发到线上的查询串。
func canonicalQuery(params [][2]string) string {
	if len(params) == 0 {
		return ""
	}
	enc := make([]string, len(params))
	for i, kv := range params {
		enc[i] = uriEncode(kv[0], true) + "=" + uriEncode(kv[1], true)
	}
	sort.Strings(enc)
	return strings.Join(enc, "&")
}

// trimHeaderValue 去掉首尾空白，并把中间连续的空格压成一个。
func trimHeaderValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// scope 返回凭据范围，例如 20130524/us-east-1/s3/aws4_request。
func (s signer) scope(t time.Time) string {
	return t.UTC().Format(dateFormat) + "/" + s.region + "/" + service + "/aws4_request"
}

// sign 计算签名。
//
//	method   HTTP 方法
//	path     已经编码好的请求路径（和线上发出去的完全一样）
//	query    canonicalQuery 的结果（预签名时不含 X-Amz-Signature）
//	headers  要签名的请求头，名字小写
//	payload  请求体的 SHA-256（十六进制），或 UNSIGNED-PAYLOAD
//
// 返回十六进制的签名和 SignedHeaders 的值。
func (s signer) sign(method, path, query string, headers map[string]string, payload string, t time.Time) (signature, signedHeaders string) {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)

	var canon strings.Builder
	canon.WriteString(method)
	canon.WriteByte('\n')
	canon.WriteString(path)
	canon.WriteByte('\n')
	canon.WriteString(query)
	canon.WriteByte('\n')
	for _, name := range names {
		canon.WriteString(name)
		canon.WriteByte(':')
		canon.WriteString(trimHeaderValue(headers[name]))
		canon.WriteByte('\n')
	}
	canon.WriteByte('\n')
	signedHeaders = strings.Join(names, ";")
	canon.WriteString(signedHeaders)
	canon.WriteByte('\n')
	canon.WriteString(payload)

	t = t.UTC()
	toSign := algorithm + "\n" + t.Format(timeFormat) + "\n" + s.scope(t) + "\n" + sha256Hex(canon.String())

	key := hmacSHA256([]byte("AWS4"+s.secretKey), t.Format(dateFormat))
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	return hex.EncodeToString(hmacSHA256(key, toSign)), signedHeaders
}

// authorization 生成 Authorization 请求头的值。
func (s signer) authorization(signature, signedHeaders string, t time.Time) string {
	return algorithm + " Credential=" + s.accessKey + "/" + s.scope(t) +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

// presignQuery 生成预签名地址的完整查询串（含签名）。
// 只签 host 一个请求头、不签请求体，所以使用者除了方法对得上以外不需要带任何特定的请求头。
func (s signer) presignQuery(method, host, path string, extra [][2]string, expires time.Duration, t time.Time) string {
	t = t.UTC()
	secs := int64(expires / time.Second)
	if secs < 1 {
		secs = 1
	}
	params := append([][2]string{
		{"X-Amz-Algorithm", algorithm},
		{"X-Amz-Credential", s.accessKey + "/" + s.scope(t)},
		{"X-Amz-Date", t.Format(timeFormat)},
		{"X-Amz-Expires", strconv.FormatInt(secs, 10)},
		{"X-Amz-SignedHeaders", "host"},
	}, extra...)
	query := canonicalQuery(params)
	signature, _ := s.sign(method, path, query, map[string]string{"host": host}, unsignedPayload, t)
	return query + "&X-Amz-Signature=" + signature
}
