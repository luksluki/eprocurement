package objectstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// s3Region is the SigV4 credential-scope region. MinIO signs server-side
// with whatever region the scope carries, so the fixed default is fine.
const s3Region = "us-east-1"

// presign mints a SigV4 query-auth URL (X-Amz-*) for method+key on the
// client-visible presign host. Presigning is pure computation — no network
// call — so the host may differ from the API endpoint the service dials
// (compose: API via minio:9000, URLs for localhost:9000).
func (m *MinIO) presign(method, objectKey string, ttl time.Duration) (string, time.Time, error) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	scope := dateStamp + "/" + s3Region + "/s3/aws4_request"

	scheme := "http"
	if m.useSSL {
		scheme = "https"
	}
	canonicalURI := "/" + uriEncode(m.bucket, false) + "/" + uriEncode(objectKey, false)

	params := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    m.accessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.FormatInt(int64(ttl/time.Second), 10),
		"X-Amz-SignedHeaders": "host",
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, uriEncode(k, true)+"="+uriEncode(params[k], true))
	}
	canonicalQuery := strings.Join(pairs, "&")

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		"host:" + m.presignHost + "\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")

	signingKey := hmacSHA256(
		hmacSHA256(
			hmacSHA256(
				hmacSHA256([]byte("AWS4"+m.secretKey), []byte(dateStamp)),
				[]byte(s3Region)),
			[]byte("s3")),
		[]byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	u := url.URL{
		Scheme:   scheme,
		Host:     m.presignHost,
		Path:     "/" + m.bucket + "/" + objectKey,
		RawQuery: canonicalQuery + "&X-Amz-Signature=" + signature,
	}
	return u.String(), now.Add(ttl), nil
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// uriEncode is RFC 3986 percent-encoding of the unreserved set; slash is
// kept for canonical URIs and escaped inside query values.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte('/')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
