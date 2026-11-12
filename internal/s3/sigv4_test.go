package s3

import (
	"strings"
	"testing"
	"time"
)

// 下面的用例来自 AWS 文档 “Signature Calculations for the Authorization Header”
// 和 “Authenticating Requests: Using Query Parameters” 里的官方示例，
// 用来确认签名算法和 AWS 的实现逐位一致。
var (
	docSigner = signer{
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
	}
	docTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	docHost = "examplebucket.s3.amazonaws.com"
)

func TestSignKnownAnswers(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		path    string
		query   [][2]string
		headers map[string]string
		payload string
		want    string
	}{
		{
			name:   "GET object with range",
			method: "GET",
			path:   "/" + uriEncode("test.txt", false),
			headers: map[string]string{
				"host":                 docHost,
				"range":                "bytes=0-9",
				"x-amz-content-sha256": emptyPayloadHash,
				"x-amz-date":           "20130524T000000Z",
			},
			payload: emptyPayloadHash,
			want:    "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name:   "PUT object",
			method: "PUT",
			path:   "/" + uriEncode("test$file.text", false),
			headers: map[string]string{
				"date":                 "Fri, 24 May 2013 00:00:00 GMT",
				"host":                 docHost,
				"x-amz-content-sha256": "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
				"x-amz-date":           "20130524T000000Z",
				"x-amz-storage-class":  "REDUCED_REDUNDANCY",
			},
			payload: "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			want:    "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name:   "GET bucket lifecycle",
			method: "GET",
			path:   "/",
			query:  [][2]string{{"lifecycle", ""}},
			headers: map[string]string{
				"host":                 docHost,
				"x-amz-content-sha256": emptyPayloadHash,
				"x-amz-date":           "20130524T000000Z",
			},
			payload: emptyPayloadHash,
			want:    "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name:   "list objects",
			method: "GET",
			path:   "/",
			query:  [][2]string{{"prefix", "J"}, {"max-keys", "2"}},
			headers: map[string]string{
				"host":                 docHost,
				"x-amz-content-sha256": emptyPayloadHash,
				"x-amz-date":           "20130524T000000Z",
			},
			payload: emptyPayloadHash,
			want:    "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := docSigner.sign(tc.method, tc.path, canonicalQuery(tc.query), tc.headers, tc.payload, docTime)
			if got != tc.want {
				t.Fatalf("signature = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPresignKnownAnswer(t *testing.T) {
	query := docSigner.presignQuery("GET", docHost, "/test.txt", nil, 86400*time.Second, docTime)
	const want = "X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if query != want {
		t.Fatalf("presigned query =\n%s\nwant\n%s", query, want)
	}
}

func TestPresignedURLShape(t *testing.T) {
	c, err := New(Config{
		Endpoint:        "https://account.r2.cloudflarestorage.com",
		Region:          "auto",
		Bucket:          "bucket",
		AccessKeyID:     "ak",
		SecretAccessKey: "sk",
		PathStyle:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	u := c.PresignUploadPart("models/_incoming/abc", "upload id+/=", 3, 15*time.Minute)
	if !strings.HasPrefix(u, "https://account.r2.cloudflarestorage.com/bucket/models/_incoming/abc?") {
		t.Fatalf("unexpected url prefix: %s", u)
	}
	for _, want := range []string{
		"partNumber=3", "uploadId=upload%20id%2B%2F%3D", "X-Amz-Expires=900",
		// 只签 host：使用者不需要带任何特定的请求头。
		"X-Amz-SignedHeaders=host&", "X-Amz-Signature=",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("url is missing %q: %s", want, u)
		}
	}
}

func TestURIEncode(t *testing.T) {
	if got := uriEncode("a b/c~d_e-f.g+h=é", false); got != "a%20b/c~d_e-f.g%2Bh%3D%C3%A9" {
		t.Fatalf("uriEncode = %q", got)
	}
	if got := uriEncode("a/b", true); got != "a%2Fb" {
		t.Fatalf("uriEncode with slash = %q", got)
	}
}
