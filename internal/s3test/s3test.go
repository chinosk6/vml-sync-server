// Package s3test 提供只给测试用的进程内假 S3 服务（不进正式程序）。
//
// 底层是 gofakes3 的内存后端；前面加了一层独立实现的 SigV4 签名校验，
// 这样测试不只验证“请求的格式对方认得”，还验证“签名和实际发出去的请求对得上”。
package s3test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// 假服务使用的固定凭据。
const (
	Bucket    = "test-bucket"
	Region    = "auto"
	AccessKey = "test-access-key"
	SecretKey = "test-secret-key"
)

// Fake 是一个跑在本进程里的假 S3 服务。
type Fake struct {
	// URL 是服务地址，可以直接填进 endpoint。
	URL string

	backend *s3mem.Backend
	server  *httptest.Server

	mu       sync.Mutex
	failNext map[string]int    // "方法 路径包含的字样" → 还要故意失败几次
	requests []string          // 见过的请求："方法 路径?查询"
	keys     map[string]string // 认可的凭据：access key → secret key
	down     bool              // 模拟整个服务不可用
	holds    []*hold           // 被卡住的请求种类
}

// hold 是一条“卡住某类请求”的规则。
type hold struct {
	method, contains string
	arrived          chan struct{} // 第一个符合的请求到了就关闭
	release          chan struct{} // 关闭后放行
	once             sync.Once
}

// New 启动假服务，测试结束时自动关闭。
func New(t testing.TB) *Fake {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket(Bucket); err != nil {
		t.Fatal(err)
	}
	f := &Fake{backend: backend, failNext: make(map[string]int), keys: map[string]string{AccessKey: SecretKey}}
	inner := gofakes3.New(backend).Server()
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		fail := false
		for key, left := range f.failNext {
			method, part, _ := strings.Cut(key, " ")
			if left > 0 && r.Method == method && strings.Contains(r.URL.RequestURI(), part) {
				f.failNext[key] = left - 1
				fail = true
				break
			}
		}
		down := f.down
		var held *hold
		for _, h := range f.holds {
			if r.Method == h.method && strings.Contains(r.URL.RequestURI(), h.contains) {
				held = h
				break
			}
		}
		f.mu.Unlock()

		if held != nil {
			held.once.Do(func() { close(held.arrived) })
			<-held.release
		}

		if down {
			writeS3Error(w, http.StatusServiceUnavailable, "ServiceUnavailable", "the fake storage is switched off")
			return
		}
		if code, err := f.verify(r); err != nil {
			writeS3Error(w, http.StatusForbidden, code, err.Error())
			return
		}
		if fail {
			writeS3Error(w, http.StatusInternalServerError, "InternalError", "injected failure")
			return
		}
		inner.ServeHTTP(w, r)
	}))
	f.URL = f.server.URL
	t.Cleanup(f.server.Close)
	return f
}

// Hold 卡住“方法是 method、地址里包含 contains”的请求：它们到了之后先不处理，等 release 被调用才继续。
// arrived 在第一个这样的请求到达时关闭。用来测“某个操作进行到一半时发生了别的事”。
func (f *Fake) Hold(method, contains string) (arrived <-chan struct{}, release func()) {
	h := &hold{method: method, contains: contains, arrived: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	f.holds = append(f.holds, h)
	f.mu.Unlock()
	var once sync.Once
	return h.arrived, func() { once.Do(func() { close(h.release) }) }
}

// SetKey 增加（或更换）一组认可的凭据。
func (f *Fake) SetKey(accessKey, secretKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[accessKey] = secretKey
}

// RemoveKey 让一组凭据失效（模拟密钥被吊销）。
func (f *Fake) RemoveKey(accessKey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.keys, accessKey)
}

// SetDown 打开或关闭“整个服务不可用”：打开时所有请求都返回 503。
func (f *Fake) SetDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// FailNext 让接下来 times 个“方法是 method、地址里包含 contains”的请求返回 500。
func (f *Fake) FailNext(method, contains string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext[method+" "+contains] = times
}

// Requests 返回到目前为止见过的全部请求（"方法 路径?查询"）。
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// Object 直接从后端读一个对象的内容；对象不存在时 ok 为 false。
func (f *Fake) Object(key string) (data []byte, ok bool) {
	obj, err := f.backend.GetObject(Bucket, key, nil)
	if err != nil {
		return nil, false
	}
	defer obj.Contents.Close()
	data, err = io.ReadAll(obj.Contents)
	if err != nil {
		return nil, false
	}
	return data, true
}

// PutObject 绕过 HTTP 直接往后端放一个对象（用来模拟“存储里凭空多出来的东西”）。
func (f *Fake) PutObject(t testing.TB, key string, data []byte) {
	t.Helper()
	if _, err := f.backend.PutObject(Bucket, key, map[string]string{}, bytes.NewReader(data), int64(len(data)), nil); err != nil {
		t.Fatal(err)
	}
}

// DeleteObject 绕过 HTTP 直接从后端删一个对象。
func (f *Fake) DeleteObject(t testing.TB, key string) {
	t.Helper()
	if _, err := f.backend.DeleteObject(Bucket, key); err != nil {
		t.Fatal(err)
	}
}

// Keys 返回后端里全部对象的名字（已排序）。
func (f *Fake) Keys(t testing.TB) []string {
	t.Helper()
	list, err := f.backend.ListBucket(Bucket, &gofakes3.Prefix{}, gofakes3.ListBucketPage{})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, c := range list.Contents {
		keys = append(keys, c.Key)
	}
	sort.Strings(keys)
	return keys
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, message)
}

// verify 校验请求的 SigV4 签名，返回要回给客户端的错误代码。
//
// verifySignature 校验请求的 SigV4 签名：请求头里的，或者预签名地址里的。
// 这里的实现故意不复用 internal/s3 的任何代码，规范化的每一步都是从“收到的请求”重新推出来的。
func (f *Fake) verify(r *http.Request) (code string, err error) {
	if err := f.verifySignature(r); err != nil {
		if errors.Is(err, errUnknownKey) {
			return "InvalidAccessKeyId", err
		}
		return "SignatureDoesNotMatch", err
	}
	return "", nil
}

var errUnknownKey = errors.New("the access key id does not exist")

func (f *Fake) verifySignature(r *http.Request) error {
	query, err := parseQuery(r.URL.RawQuery)
	if err != nil {
		return err
	}

	var credential, signedHeaders, signature, amzDate, payload string
	presigned := false
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "AWS4-HMAC-SHA256 "
		if !strings.HasPrefix(auth, prefix) {
			return fmt.Errorf("unsupported Authorization scheme")
		}
		for _, field := range strings.Split(auth[len(prefix):], ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(field), "=")
			switch k {
			case "Credential":
				credential = v
			case "SignedHeaders":
				signedHeaders = v
			case "Signature":
				signature = v
			}
		}
		amzDate = r.Header.Get("X-Amz-Date")
		payload = r.Header.Get("X-Amz-Content-Sha256")
		if payload == "" {
			return fmt.Errorf("missing x-amz-content-sha256")
		}
		// 请求头里声明了请求体的哈希，就真的算一遍对一对。
		if payload != "UNSIGNED-PAYLOAD" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(body)
			if hex.EncodeToString(sum[:]) != payload {
				return fmt.Errorf("payload hash does not match the body")
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
	} else if sig := lookup(query, "X-Amz-Signature"); sig != "" {
		presigned = true
		signature = sig
		credential = lookup(query, "X-Amz-Credential")
		signedHeaders = lookup(query, "X-Amz-SignedHeaders")
		amzDate = lookup(query, "X-Amz-Date")
		payload = "UNSIGNED-PAYLOAD"
		if lookup(query, "X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
			return fmt.Errorf("unsupported algorithm")
		}
		// 约定：临时地址只要求方法对得上，不能要求任何特定的请求头。
		if signedHeaders != "host" {
			return fmt.Errorf("presigned url signs headers other than host: %s", signedHeaders)
		}
		issued, err := time.Parse("20060102T150405Z", amzDate)
		if err != nil {
			return fmt.Errorf("bad X-Amz-Date")
		}
		secs, err := strconv.Atoi(lookup(query, "X-Amz-Expires"))
		if err != nil || secs <= 0 {
			return fmt.Errorf("bad X-Amz-Expires")
		}
		if time.Now().After(issued.Add(time.Duration(secs) * time.Second)) {
			return fmt.Errorf("presigned url has expired")
		}
	} else {
		return fmt.Errorf("request is not signed")
	}

	// 凭据范围：<access key>/<日期>/<区域>/s3/aws4_request
	scope := strings.Split(credential, "/")
	if len(scope) != 5 || scope[2] != Region || scope[3] != "s3" || scope[4] != "aws4_request" {
		return fmt.Errorf("bad credential scope")
	}
	f.mu.Lock()
	secret, known := f.keys[scope[0]]
	f.mu.Unlock()
	if !known {
		return errUnknownKey
	}
	if !strings.HasPrefix(amzDate, scope[1]) {
		return fmt.Errorf("date does not match credential scope")
	}

	var pairs []string
	for _, kv := range query {
		if presigned && kv[0] == "X-Amz-Signature" {
			continue
		}
		pairs = append(pairs, awsEscape(kv[0])+"="+awsEscape(kv[1]))
	}
	sort.Strings(pairs)

	var canon strings.Builder
	canon.WriteString(r.Method + "\n" + r.URL.EscapedPath() + "\n" + strings.Join(pairs, "&") + "\n")
	for _, name := range strings.Split(signedHeaders, ";") {
		value := r.Host
		if name != "host" {
			values := r.Header.Values(name)
			if len(values) == 0 {
				return fmt.Errorf("signed header %s is missing", name)
			}
			value = strings.Join(values, ",")
		}
		canon.WriteString(name + ":" + strings.Join(strings.Fields(value), " ") + "\n")
	}
	canon.WriteString("\n" + signedHeaders + "\n" + payload)

	canonHash := sha256.Sum256([]byte(canon.String()))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + strings.Join(scope[1:], "/") + "\n" + hex.EncodeToString(canonHash[:])
	key := []byte("AWS4" + secret)
	for _, step := range []string{scope[1], scope[2], scope[3], scope[4], toSign} {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(step))
		key = mac.Sum(nil)
	}
	if !hmac.Equal([]byte(hex.EncodeToString(key)), []byte(signature)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// parseQuery 把查询串拆成解码后的键值对（保留顺序和重复的键）。
func parseQuery(raw string) ([][2]string, error) {
	var out [][2]string
	if raw == "" {
		return out, nil
	}
	for _, item := range strings.Split(raw, "&") {
		k, v, _ := strings.Cut(item, "=")
		dk, err := url.QueryUnescape(k)
		if err != nil {
			return nil, err
		}
		dv, err := url.QueryUnescape(v)
		if err != nil {
			return nil, err
		}
		out = append(out, [2]string{dk, dv})
	}
	return out, nil
}

func lookup(query [][2]string, key string) string {
	for _, kv := range query {
		if kv[0] == key {
			return kv[1]
		}
	}
	return ""
}

// awsEscape 按 SigV4 的规则编码：在标准库的查询串编码基础上，把空格改回 %20、把 ~ 还原。
func awsEscape(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "%7E", "~")
	e = strings.ReplaceAll(e, "*", "%2A")
	return e
}
