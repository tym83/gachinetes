package replica

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Bucket is a prefix of an S3-compatible bucket, reached by path-style URLs
// (endpoint/bucket/key) and requests signed with AWS Signature Version 4:
// what kuberoot's state backups use, with the reads, listings and deletes
// a journal needs besides the uploads.
type Bucket struct {
	Endpoint  *url.URL
	Region    string
	Name      string
	Prefix    string
	AccessKey string
	SecretKey string
	Client    *http.Client
	// now is the signing clock, for tests.
	now func() time.Time
}

// ParseBucket reads s3://bucket[/prefix] with the endpoint and region given
// apart and the keys from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. An
// empty endpoint is AWS's, for the region.
func ParseBucket(location, endpoint, region string) (*Bucket, error) {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return nil, fmt.Errorf("%q: s3://bucket[/prefix] expected", location)
	}
	if region == "" {
		region = "us-east-1"
	}
	if endpoint == "" {
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	ep, err := url.Parse(endpoint)
	if err != nil || (ep.Scheme != "http" && ep.Scheme != "https") || ep.Host == "" {
		return nil, fmt.Errorf("endpoint %q: an http(s) URL expected", endpoint)
	}
	b := &Bucket{Endpoint: ep, Region: region, Name: u.Host, Prefix: strings.Trim(u.Path, "/"),
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY")}
	if b.AccessKey == "" || b.SecretKey == "" {
		return nil, fmt.Errorf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required")
	}
	return b, nil
}

// String is the bucket as s3://bucket/prefix.
func (b *Bucket) String() string { return "s3://" + strings.TrimSuffix(b.Name+"/"+b.Prefix, "/") }

func (b *Bucket) key(name string) string {
	if b.Prefix == "" {
		return name
	}
	return b.Prefix + "/" + name
}

// Object is an object under the prefix, its name relative to it.
type Object struct {
	Name string
	Size int64
}

// Put uploads body as name.
func (b *Bucket) Put(ctx context.Context, name string, body []byte) error {
	resp, err := b.do(ctx, http.MethodPut, "/"+b.key(name), nil, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Get downloads name.
func (b *Bucket) Get(ctx context.Context, name string) ([]byte, error) {
	resp, err := b.do(ctx, http.MethodGet, "/"+b.key(name), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Delete deletes name.
func (b *Bucket) Delete(ctx context.Context, name string) error {
	resp, err := b.do(ctx, http.MethodDelete, "/"+b.key(name), nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// List is every object whose name starts with under, by name.
func (b *Bucket) List(ctx context.Context, under string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {b.key(under)}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := b.do(ctx, http.MethodGet, "/", q, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key  string
				Size int64
			}
			IsTruncated           bool
			NextContinuationToken string
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", b, err)
		}
		for _, c := range page.Contents {
			name := c.Key
			if b.Prefix != "" {
				name = strings.TrimPrefix(name, b.Prefix+"/")
			}
			out = append(out, Object{Name: name, Size: c.Size})
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			break
		}
		token = page.NextContinuationToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// do sends a signed request for path under the bucket and fails on any
// answer but 2xx.
func (b *Bucket) do(ctx context.Context, method, path string, q url.Values, body []byte) (*http.Response, error) {
	u := *b.Endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + b.Name + path
	if path == "/" {
		u.Path = strings.TrimSuffix(u.Path, "/")
	}
	u.RawQuery = canonicalQuery(q)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	sum := sha256.Sum256(body)
	now := time.Now
	if b.now != nil {
		now = b.now
	}
	b.sign(req, hex.EncodeToString(sum[:]), now())
	c := b.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, &StatusError{Method: method, Path: path, Status: resp.StatusCode,
			Message: fmt.Sprintf("%s %s%s: %s: %s", method, b.Name, path, resp.Status, strings.TrimSpace(string(msg)))}
	}
	return resp, nil
}

// StatusError is an answer of the storage other than 2xx.
type StatusError struct {
	Method, Path, Message string
	Status                int
}

func (e *StatusError) Error() string { return e.Message }

// canonicalQuery is the query as SigV4 signs it: names and values
// escaped, sorted by name.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// sign adds the date, payload hash and Authorization headers, as
// kuberoot's s3put does.
func (b *Bucket) sign(req *http.Request, payloadSHA256 string, now time.Time) {
	stamp := now.UTC().Format("20060102T150405Z")
	day := stamp[:8]
	req.Header.Set("x-amz-date", stamp)
	req.Header.Set("x-amz-content-sha256", payloadSHA256)
	headers := map[string]string{"host": req.URL.Host}
	for name, values := range req.Header {
		l := strings.ToLower(name)
		if strings.HasPrefix(l, "x-amz-") || l == "content-type" {
			headers[l] = strings.TrimSpace(strings.Join(values, ","))
		}
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + headers[n] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{req.Method, uriEncode(req.URL.Path, false), req.URL.RawQuery,
		canonHeaders.String(), signed, payloadSHA256}, "\n")
	scope := day + "/" + b.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hexSum([]byte(canonical))
	key := hmacSum([]byte("AWS4"+b.SecretKey), day)
	for _, part := range []string{b.Region, "s3", "aws4_request"} {
		key = hmacSum(key, part)
	}
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s,SignedHeaders=%s,Signature=%s",
		b.AccessKey, scope, signed, hex.EncodeToString(hmacSum(key, toSign))))
}

// uriEncode escapes every byte but the unreserved ones, and the slashes
// unless slash is set.
func uriEncode(s string, slash bool) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-' || c == '_' || c == '.' || c == '~' ||
			('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9'):
			out.WriteByte(c)
		case c == '/' && !slash:
			out.WriteByte(c)
		default:
			fmt.Fprintf(&out, "%%%02X", c)
		}
	}
	return out.String()
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hmacSum(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
