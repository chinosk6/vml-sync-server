package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config 是连接一个 S3 兼容服务需要的全部信息。
type Config struct {
	Endpoint        string // 例如 https://<账户ID>.r2.cloudflarestorage.com，不带路径
	Region          string // R2 用 "auto"
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	PathStyle       bool         // true：地址是 endpoint/bucket/key；false：bucket.endpoint/key
	HTTPClient      *http.Client // 可选，留空用内置的
}

// Client 是 S3 兼容服务的客户端，绑定在一个桶上。可以并发使用。
type Client struct {
	scheme   string
	host     string // 可能带端口；虚拟主机风格时已经含桶名
	basePath string // path-style 时是 "/<bucket>"，否则为空
	sig      signer
	hc       *http.Client
	now      func() time.Time
}

// Object 是列表里的一个对象。
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Part 是分片上传里已经传好的一片。
type Part struct {
	Number int
	ETag   string
	Size   int64
}

// Upload 是一个还没完成的分片上传。
type Upload struct {
	Key      string
	UploadID string
}

// Error 是服务端返回的错误。
type Error struct {
	Op      string
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("s3 %s: %d %s: %s", e.Op, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("s3 %s: unexpected status %d", e.Op, e.Status)
}

// IsNotFound 报告错误是不是“对象（或分片上传）不存在”。
func IsNotFound(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code {
	case "NoSuchKey", "NoSuchUpload", "NotFound":
		return true
	}
	return e.Status == http.StatusNotFound && e.Code == ""
}

// New 按配置创建客户端。
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("s3: endpoint must look like https://host[:port]")
	}
	if u.Path != "" {
		return nil, errors.New("s3: endpoint must not contain a path")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket is empty")
	}
	host := u.Host
	// 去掉默认端口，免得 Host 请求头和签名里的 host 出现两种写法。
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = u.Hostname()
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	c := &Client{
		scheme: u.Scheme,
		host:   host,
		sig: signer{
			accessKey: cfg.AccessKeyID,
			secretKey: cfg.SecretAccessKey,
			region:    cfg.Region,
		},
		hc:  cfg.HTTPClient,
		now: time.Now,
	}
	if cfg.PathStyle {
		c.basePath = "/" + uriEncode(cfg.Bucket, true)
	} else {
		c.host = cfg.Bucket + "." + host
	}
	if c.hc == nil {
		c.hc = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			ExpectContinueTimeout: time.Second,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
			// 对象内容必须逐字节原样往返，不要让传输层自作主张地压缩 / 解压。
			DisableCompression: true,
		}}
	}
	return c, nil
}

// objectPath 返回对象的请求路径（已编码）。
func (c *Client) objectPath(key string) string {
	return c.basePath + "/" + uriEncode(key, false)
}

// bucketPath 返回桶本身的请求路径。
func (c *Client) bucketPath() string {
	if c.basePath == "" {
		return "/"
	}
	return c.basePath
}

// request 描述一次要发出去的调用。
type request struct {
	op      string
	method  string
	path    string            // 已编码
	query   [][2]string       // 未编码的键值
	headers map[string]string // 额外请求头（全部参与签名）
	body    io.Reader
	size    int64  // body 的长度
	payload string // 请求体的 SHA-256；空 = 没有请求体
}

// do 签名并发出请求。返回的响应状态码一定是 2xx；其余情况都转成错误。
func (c *Client) do(ctx context.Context, rq request) (*http.Response, error) {
	query := canonicalQuery(rq.query)
	target := c.scheme + "://" + c.host + rq.path
	if query != "" {
		target += "?" + query
	}
	body := rq.body
	if body == nil || rq.size == 0 {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, rq.method, target, body)
	if err != nil {
		return nil, fmt.Errorf("s3 %s: %w", rq.op, err)
	}
	req.ContentLength = rq.size
	payload := rq.payload
	if payload == "" {
		payload = emptyPayloadHash
	}

	now := c.now()
	signed := map[string]string{
		"host":                 req.URL.Host,
		"x-amz-content-sha256": payload,
		"x-amz-date":           now.UTC().Format(timeFormat),
	}
	for name, value := range rq.headers {
		signed[strings.ToLower(name)] = value
	}
	for name, value := range signed {
		if name != "host" {
			req.Header.Set(name, value)
		}
	}
	signature, signedHeaders := c.sig.sign(rq.method, rq.path, query, signed, payload, now)
	req.Header.Set("Authorization", c.sig.authorization(signature, signedHeaders, now))

	resp, err := c.hc.Do(req)
	if err != nil {
		// url.Error 的文字里带完整地址；只留下原因。
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("s3 %s: %w", rq.op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, parseError(rq.op, resp)
	}
	return resp, nil
}

// errorBody 是 S3 的 XML 错误响应。
type errorBody struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func parseError(op string, resp *http.Response) error {
	e := &Error{Op: op, Status: resp.StatusCode}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body errorBody
	if xml.Unmarshal(raw, &body) == nil {
		e.Code = body.Code
		e.Message = body.Message
	}
	return e
}

// readXML 读完响应体并解析。有些操作（完成分片上传、复制）会在 200 的响应体里报错，这里一并处理。
func readXML(op string, resp *http.Response, into any) error {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("s3 %s: reading response: %w", op, err)
	}
	var body errorBody
	if xml.Unmarshal(raw, &body) == nil && body.Code != "" {
		return &Error{Op: op, Status: resp.StatusCode, Code: body.Code, Message: body.Message}
	}
	if into == nil {
		return nil
	}
	if err := xml.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("s3 %s: cannot parse response: %w", op, err)
	}
	return nil
}

func discard(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// PutObject 上传一个对象。sha256Hex 是内容的 SHA-256（十六进制），会被签进请求里，
// 服务端据此拒收传输中损坏的内容。
func (c *Client) PutObject(ctx context.Context, key string, body io.Reader, size int64, sha256Hex string) error {
	resp, err := c.do(ctx, request{
		op:      "PutObject",
		method:  http.MethodPut,
		path:    c.objectPath(key),
		headers: map[string]string{"content-type": "application/octet-stream"},
		body:    body,
		size:    size,
		payload: sha256Hex,
	})
	if err != nil {
		return err
	}
	discard(resp)
	return nil
}

// GetObject 读取对象从 offset 开始的 length 个字节；length < 0 表示读到结尾。
func (c *Client) GetObject(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	rq := request{op: "GetObject", method: http.MethodGet, path: c.objectPath(key)}
	ranged := offset > 0 || length >= 0
	if ranged {
		spec := "bytes=" + strconv.FormatInt(offset, 10) + "-"
		if length >= 0 {
			if length == 0 {
				return io.NopCloser(bytes.NewReader(nil)), nil
			}
			spec += strconv.FormatInt(offset+length-1, 10)
		}
		rq.headers = map[string]string{"range": spec}
	}
	resp, err := c.do(ctx, rq)
	if err != nil {
		return nil, err
	}
	if !ranged || resp.StatusCode == http.StatusPartialContent {
		return resp.Body, nil
	}
	// 对方无视了 Range、把整个对象发了回来：自己跳过前面的部分。
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("s3 GetObject: skipping to offset: %w", err)
		}
	}
	if length < 0 {
		return resp.Body, nil
	}
	return &limitedBody{Reader: io.LimitReader(resp.Body, length), Closer: resp.Body}, nil
}

type limitedBody struct {
	io.Reader
	io.Closer
}

// HeadObject 返回对象的大小和修改时间。
func (c *Client) HeadObject(ctx context.Context, key string) (Object, error) {
	resp, err := c.do(ctx, request{op: "HeadObject", method: http.MethodHead, path: c.objectPath(key)})
	if err != nil {
		return Object{}, err
	}
	resp.Body.Close()
	obj := Object{Key: key, Size: resp.ContentLength}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		obj.LastModified = t
	}
	if obj.Size < 0 {
		return Object{}, errors.New("s3 HeadObject: response has no Content-Length")
	}
	return obj, nil
}

// DeleteObject 删除对象；对象不存在不算错。
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	resp, err := c.do(ctx, request{op: "DeleteObject", method: http.MethodDelete, path: c.objectPath(key)})
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	discard(resp)
	return nil
}

// CopyObject 在桶内把 srcKey 复制成 dstKey（服务端复制，内容不经过本机）。
func (c *Client) CopyObject(ctx context.Context, srcKey, dstKey string) error {
	resp, err := c.do(ctx, request{
		op:      "CopyObject",
		method:  http.MethodPut,
		path:    c.objectPath(dstKey),
		headers: map[string]string{"x-amz-copy-source": c.copySource(srcKey)},
	})
	if err != nil {
		return err
	}
	return readXML("CopyObject", resp, nil)
}

// copySource 返回 x-amz-copy-source 的值：/桶名/对象名（已编码）。
func (c *Client) copySource(key string) string {
	bucket := c.basePath
	if bucket == "" {
		// 虚拟主机风格：桶名是 host 的第一段。
		bucket = "/" + uriEncode(strings.SplitN(c.host, ".", 2)[0], true)
	}
	return bucket + "/" + uriEncode(key, false)
}

type listObjectsResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
}

// ListObjects 列出名字以 prefix 开头的全部对象（自动翻页）。
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		query := [][2]string{{"list-type", "2"}, {"max-keys", "1000"}}
		if prefix != "" {
			query = append(query, [2]string{"prefix", prefix})
		}
		if token != "" {
			query = append(query, [2]string{"continuation-token", token})
		}
		resp, err := c.do(ctx, request{op: "ListObjectsV2", method: http.MethodGet, path: c.bucketPath(), query: query})
		if err != nil {
			return nil, err
		}
		var page listObjectsResult
		if err := readXML("ListObjectsV2", resp, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Contents {
			obj := Object{Key: item.Key, Size: item.Size}
			if t, err := time.Parse(time.RFC3339, item.LastModified); err == nil {
				obj.LastModified = t
			}
			out = append(out, obj)
		}
		if !page.IsTruncated || page.NextContinuationToken == "" || page.NextContinuationToken == token {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// CreateMultipartUpload 开始一个分片上传，返回它的 UploadId。
func (c *Client) CreateMultipartUpload(ctx context.Context, key string) (string, error) {
	resp, err := c.do(ctx, request{
		op:      "CreateMultipartUpload",
		method:  http.MethodPost,
		path:    c.objectPath(key),
		query:   [][2]string{{"uploads", ""}},
		headers: map[string]string{"content-type": "application/octet-stream"},
	})
	if err != nil {
		return "", err
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := readXML("CreateMultipartUpload", resp, &out); err != nil {
		return "", err
	}
	if out.UploadID == "" {
		return "", errors.New("s3 CreateMultipartUpload: response has no UploadId")
	}
	return out.UploadID, nil
}

type listPartsResult struct {
	IsTruncated          bool `xml:"IsTruncated"`
	NextPartNumberMarker int  `xml:"NextPartNumberMarker"`
	Parts                []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
		Size       int64  `xml:"Size"`
	} `xml:"Part"`
}

// ListParts 列出一个分片上传里已经传好的全部分片（自动翻页，按片号升序）。
func (c *Client) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	var out []Part
	marker := 0
	for {
		query := [][2]string{{"uploadId", uploadID}, {"max-parts", "1000"}}
		if marker > 0 {
			query = append(query, [2]string{"part-number-marker", strconv.Itoa(marker)})
		}
		resp, err := c.do(ctx, request{op: "ListParts", method: http.MethodGet, path: c.objectPath(key), query: query})
		if err != nil {
			return nil, err
		}
		var page listPartsResult
		if err := readXML("ListParts", resp, &page); err != nil {
			return nil, err
		}
		for _, p := range page.Parts {
			out = append(out, Part{Number: p.PartNumber, ETag: p.ETag, Size: p.Size})
		}
		if !page.IsTruncated {
			return out, nil
		}
		if page.NextPartNumberMarker <= marker {
			return nil, errors.New("s3 ListParts: truncated listing did not advance")
		}
		marker = page.NextPartNumberMarker
	}
}

// CompleteMultipartUpload 把 parts（必须按片号升序）拼成最终对象。
func (c *Client) CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []Part) error {
	var body bytes.Buffer
	body.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		body.WriteString("<Part><PartNumber>")
		body.WriteString(strconv.Itoa(p.Number))
		body.WriteString("</PartNumber><ETag>")
		xml.EscapeText(&body, []byte(p.ETag))
		body.WriteString("</ETag></Part>")
	}
	body.WriteString("</CompleteMultipartUpload>")
	raw := body.Bytes()

	resp, err := c.do(ctx, request{
		op:      "CompleteMultipartUpload",
		method:  http.MethodPost,
		path:    c.objectPath(key),
		query:   [][2]string{{"uploadId", uploadID}},
		headers: map[string]string{"content-type": "application/xml"},
		body:    bytes.NewReader(raw),
		size:    int64(len(raw)),
		payload: sha256Hex(string(raw)),
	})
	if err != nil {
		return err
	}
	return readXML("CompleteMultipartUpload", resp, nil)
}

// AbortMultipartUpload 放弃一个分片上传并丢掉已经传的分片；上传不存在不算错。
func (c *Client) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	resp, err := c.do(ctx, request{
		op:     "AbortMultipartUpload",
		method: http.MethodDelete,
		path:   c.objectPath(key),
		query:  [][2]string{{"uploadId", uploadID}},
	})
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	discard(resp)
	return nil
}

type listUploadsResult struct {
	IsTruncated        bool   `xml:"IsTruncated"`
	NextKeyMarker      string `xml:"NextKeyMarker"`
	NextUploadIDMarker string `xml:"NextUploadIdMarker"`
	Uploads            []struct {
		Key      string `xml:"Key"`
		UploadID string `xml:"UploadId"`
	} `xml:"Upload"`
}

// ListMultipartUploads 列出名字以 prefix 开头、还没完成的分片上传（自动翻页）。
func (c *Client) ListMultipartUploads(ctx context.Context, prefix string) ([]Upload, error) {
	var out []Upload
	keyMarker, idMarker := "", ""
	for {
		query := [][2]string{{"uploads", ""}, {"max-uploads", "1000"}}
		if prefix != "" {
			query = append(query, [2]string{"prefix", prefix})
		}
		if keyMarker != "" {
			query = append(query, [2]string{"key-marker", keyMarker})
			if idMarker != "" {
				query = append(query, [2]string{"upload-id-marker", idMarker})
			}
		}
		resp, err := c.do(ctx, request{op: "ListMultipartUploads", method: http.MethodGet, path: c.bucketPath(), query: query})
		if err != nil {
			// 有的实现在一个分片上传都没有时返回“不存在”，等同于空列表。
			if IsNotFound(err) {
				return out, nil
			}
			return nil, err
		}
		var page listUploadsResult
		if err := readXML("ListMultipartUploads", resp, &page); err != nil {
			return nil, err
		}
		for _, u := range page.Uploads {
			out = append(out, Upload{Key: u.Key, UploadID: u.UploadID})
		}
		if !page.IsTruncated || page.NextKeyMarker == "" ||
			(page.NextKeyMarker == keyMarker && page.NextUploadIDMarker == idMarker) {
			return out, nil
		}
		keyMarker, idMarker = page.NextKeyMarker, page.NextUploadIDMarker
	}
}

// presign 生成一个预签名地址。
func (c *Client) presign(method, key string, extra [][2]string, ttl time.Duration) string {
	path := c.objectPath(key)
	return c.scheme + "://" + c.host + path + "?" + c.sig.presignQuery(method, c.host, path, extra, ttl, c.now())
}

// PresignUploadPart 生成上传某一片用的临时地址：对它发 PUT、请求体是这一片的内容即可，不需要任何额外请求头。
func (c *Client) PresignUploadPart(key, uploadID string, part int, ttl time.Duration) string {
	return c.presign(http.MethodPut, key, [][2]string{
		{"partNumber", strconv.Itoa(part)},
		{"uploadId", uploadID},
	}, ttl)
}

// PresignGetObject 生成下载对象用的临时地址。
func (c *Client) PresignGetObject(key string, ttl time.Duration) string {
	return c.presign(http.MethodGet, key, nil, ttl)
}
