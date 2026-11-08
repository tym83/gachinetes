package replica

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
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

	"filippo.io/age"

	"github.com/tym83/gachinetes/pkg/tape"
)

// fakeTape is a store's tape in memory, served as /tape/v1 serves it: the
// versions in revision order, each revision at a time, and where the tape
// starts.
type fakeTape struct {
	mu       sync.Mutex
	versions []tape.KV
	current  int64
	from     int64
	clock    int64
}

func newFakeTape() *fakeTape { return &fakeTape{current: 1, from: 1, clock: 1_700_000_000_000} }

type op struct {
	key, value string
	del        bool
}

func put(k, v string) op { return op{key: k, value: v} }
func del(k string) op    { return op{key: k, del: true} }

// write commits ops at one new revision, a second after the last.
func (f *fakeTape) write(ops ...op) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current++
	f.clock += 1000
	for _, o := range ops {
		prev := f.latest(o.key, f.current-1)
		if o.del {
			if prev != nil {
				f.versions = append(f.versions, tape.KV{Key: o.key, ModRevision: f.current, Deleted: true, Time: f.clock})
			}
			continue
		}
		kv := tape.KV{Key: o.key, Value: []byte(o.value), CreateRevision: f.current, ModRevision: f.current, Version: 1, Time: f.clock}
		if prev != nil {
			kv.CreateRevision, kv.Version = prev.CreateRevision, prev.Version+1
		}
		f.versions = append(f.versions, kv)
	}
	return f.current
}

func (f *fakeTape) latest(key string, rev int64) *tape.KV {
	var out *tape.KV
	for i := range f.versions {
		v := f.versions[i]
		if v.Key == key && v.ModRevision <= rev {
			out = &f.versions[i]
		}
	}
	if out == nil || out.Deleted {
		return nil
	}
	return out
}

// stateAt is every key as it was at rev.
func (f *fakeTape) stateAt(rev int64) map[string]tape.KV {
	f.mu.Lock()
	defer f.mu.Unlock()
	return replay(f.versions, rev)
}

func replay(versions []tape.KV, rev int64) map[string]tape.KV {
	out := map[string]tape.KV{}
	for _, v := range versions {
		if v.ModRevision > rev {
			continue
		}
		if v.Deleted {
			delete(out, v.Key)
			continue
		}
		v.Time = 0
		out[v.Key] = v
	}
	return out
}

func (f *fakeTape) timeOf(rev int64) int64 {
	var at int64
	for _, v := range f.versions {
		if v.ModRevision <= rev {
			at = v.Time
		}
	}
	return at
}

func (f *fakeTape) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	out := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch r.URL.Path {
	case "/tape/v1/info":
		out(tape.Info{Current: f.current, From: f.from, Started: true})
	case "/tape/v1/range":
		rev, _ := strconv.ParseInt(q.Get("rev"), 10, 64)
		if q.Get("key") != "" || q.Get("end") != "\x00" {
			http.Error(w, "the fake reads every key only", http.StatusBadRequest)
			return
		}
		if rev < f.from || rev > f.current {
			http.Error(w, "out of the tape", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		st := replay(f.versions, rev)
		res := tape.Range{Revision: rev, Time: f.timeOf(rev), KVs: []tape.KV{}}
		for _, kv := range st {
			kv.Time = f.timeOf(kv.ModRevision)
			res.KVs = append(res.KVs, kv)
		}
		sort.Slice(res.KVs, func(i, j int) bool { return res.KVs[i].Key < res.KVs[j].Key })
		out(res)
	case "/tape/v1/journal":
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		if after < f.from || after > f.current {
			http.Error(w, "out of the tape", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		j := tape.Journal{After: after, Upto: f.current, KVs: []tape.KV{}}
		for i, v := range f.versions {
			if v.ModRevision <= after {
				continue
			}
			if limit > 0 && len(j.KVs) >= limit && v.ModRevision != j.KVs[len(j.KVs)-1].ModRevision {
				j.More, j.Upto = true, j.KVs[len(j.KVs)-1].ModRevision
				break
			}
			j.KVs = append(j.KVs, f.versions[i])
		}
		out(j)
	default:
		http.NotFound(w, r)
	}
}

// fakeS3 is a bucket in memory behind path-style URLs, checking that each
// request is signed and its payload hash is right.
type fakeS3 struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
	puts    int
}

func (s *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=test/") || !strings.Contains(auth, "Signature=") ||
		r.Header.Get("x-amz-content-sha256") != hex.EncodeToString(sum[:]) {
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/"+s.bucket)
	if path == "" && r.Method == http.MethodGet {
		prefix := r.URL.Query().Get("prefix")
		type content struct{ Key string }
		var res struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []content
		}
		for k := range s.objects {
			if strings.HasPrefix(k, prefix) {
				res.Contents = append(res.Contents, content{Key: k})
			}
		}
		_ = xml.NewEncoder(w).Encode(res)
		return
	}
	key := strings.TrimPrefix(path, "/")
	switch r.Method {
	case http.MethodPut:
		s.objects[key] = body
		s.puts++
	case http.MethodGet:
		v, ok := s.objects[key]
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		_, _ = w.Write(v)
	case http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *fakeS3) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type rig struct {
	tape   *fakeTape
	s3     *fakeS3
	bucket *Bucket
	ship   *Shipper
	id     *age.X25519Identity
	now    time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ft := newFakeTape()
	ts := httptest.NewServer(ft)
	t.Cleanup(ts.Close)
	fs := &fakeS3{bucket: "tapes", objects: map[string][]byte{}}
	ss := httptest.NewServer(fs)
	t.Cleanup(ss.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	b, err := ParseBucket("s3://tapes/clusters/gym", ss.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{tape: ft, s3: fs, bucket: b, id: id, now: time.UnixMilli(ft.clock)}
	r.ship = &Shipper{Tape: &tape.Store{URL: ts.URL}, Bucket: b, Recipients: []age.Recipient{id.Recipient()},
		Page: 3, Now: func() time.Time { return r.now }, Logf: t.Logf}
	return r
}

func (r *rig) sync(t *testing.T) SyncResult {
	t.Helper()
	res, err := r.ship.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// readTapeFile reads a tape file back: its header and versions.
func readTapeFile(t *testing.T, raw []byte) (TapeFileHeader, []tape.KV) {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(raw))
	var h TapeFileHeader
	var kvs []tape.KV
	for i := 0; sc.Scan(); i++ {
		if i == 0 {
			if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var kv tape.KV
		if err := json.Unmarshal(sc.Bytes(), &kv); err != nil {
			t.Fatal(err)
		}
		kvs = append(kvs, kv)
	}
	if h.Format != TapeFileFormat {
		t.Fatalf("header: %+v", h)
	}
	return h, kvs
}

func (r *rig) restore(t *testing.T, target Target) (Restored, []byte) {
	t.Helper()
	var buf bytes.Buffer
	res, err := WriteTapeFile(context.Background(), r.bucket, []age.Identity{r.id}, target, &buf)
	if err != nil {
		t.Fatalf("restore to %+v: %v", target, err)
	}
	return res, buf.Bytes()
}

// The cluster is lost after shipping; the tape file from the bucket holds
// every revision from the base to the one asked as the cluster had it.
func TestRestoreAtARevisionFromTheBucket(t *testing.T) {
	r := newRig(t)
	r.tape.write(put("/registry/configmaps/shop/a", "1"), put("/registry/configmaps/shop/b", "1"))
	r.tape.write(put("/registry/configmaps/shop/a", "2"))
	if res := r.sync(t); res.Base != 1 || res.Shipped != 3 {
		t.Fatalf("first pass: %+v", res)
	}
	r.tape.write(del("/registry/configmaps/shop/b"))
	mid := r.tape.write(put("/registry/secrets/shop/s", "k8s:enc:secretbox:v1:key1:..."), put("/registry/configmaps/shop/c", "1"))
	r.tape.write(put("/registry/configmaps/shop/a", "3"), del("/registry/configmaps/shop/c"))
	r.tape.write(put("/registry/configmaps/shop/b", "again"))
	if res := r.sync(t); res.Shipped != 7 || res.Versions != 6 {
		t.Fatalf("second pass: %+v", res)
	}
	for _, n := range r.s3.names() {
		if !strings.HasPrefix(n, "clusters/gym/") || !strings.HasSuffix(n, ".jsonl.gz.age") {
			t.Fatalf("object %s: not under the prefix or not encrypted", n)
		}
		if bytes.Contains(r.s3.objects[n], []byte("configmaps")) {
			t.Fatalf("object %s holds keys in the clear", n)
		}
	}
	if res := r.sync(t); res.Versions != 0 || res.Base != 0 {
		t.Fatalf("a pass with nothing new shipped %+v", res)
	}

	for _, target := range []int64{mid, 7, 2} {
		res, raw := r.restore(t, Target{Revision: target})
		h, kvs := readTapeFile(t, raw)
		if h.Base != 1 || res.Revision != target {
			t.Fatalf("restore to %d: base %d, up to %d", target, h.Base, res.Revision)
		}
		for rev := h.Base; rev <= target; rev++ {
			want, got := r.tape.stateAt(rev), replay(kvs, rev)
			if fmt.Sprint(want) != fmt.Sprint(got) {
				t.Fatalf("restore to %d, revision %d:\nwant %v\ngot  %v", target, rev, want, got)
			}
		}
		for _, kv := range kvs {
			if kv.ModRevision > target {
				t.Fatalf("restore to %d holds revision %d", target, kv.ModRevision)
			}
		}
	}

	// At a moment: the newest revision written at or before it.
	res, _ := r.restore(t, Target{At: time.UnixMilli(r.tape.timeOf(mid) + 500)})
	if res.Revision != mid {
		t.Fatalf("restore to the moment after %d: up to %d", mid, res.Revision)
	}
	if _, err := WriteTapeFile(context.Background(), r.bucket, nil, Target{}, io.Discard); err == nil {
		t.Fatal("restored an encrypted tape without an identity")
	}
	if _, err := WriteTapeFile(context.Background(), r.bucket, []age.Identity{r.id}, Target{Revision: 99}, io.Discard); err == nil {
		t.Fatal("restored past what the bucket holds")
	}
}

// A new base comes every BaseEvery; retention drops what is older than the
// newest base at least Retention old, and the tape still restores whole
// from there.
func TestRetentionKeepsAnUnbrokenTape(t *testing.T) {
	r := newRig(t)
	r.ship.BaseEvery, r.ship.Retention = time.Hour, 3*time.Hour
	for hour := 0; hour < 8; hour++ {
		r.tape.write(put("/registry/configmaps/shop/a", strconv.Itoa(hour)))
		r.tape.clock += int64(time.Hour / time.Millisecond)
		r.now = time.UnixMilli(r.tape.clock)
		r.sync(t)
	}
	cat, err := r.bucket.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Bases) < 2 || len(cat.Bases) > 5 {
		t.Fatalf("bases kept: %+v", cat.Bases)
	}
	oldest := cat.Bases[0]
	if age := r.now.Sub(time.UnixMilli(oldest.Time)); age < 3*time.Hour || age > 5*time.Hour {
		t.Fatalf("the oldest base is %s old", age)
	}
	for _, s := range cat.Segments {
		if s.Upto <= oldest.Revision {
			t.Fatalf("segment %s kept before the oldest base", s.Name)
		}
	}
	res, raw := r.restore(t, Target{})
	h, kvs := readTapeFile(t, raw)
	if h.Base != oldest.Revision || res.Revision != r.tape.current {
		t.Fatalf("restore: base %d (oldest %d), up to %d of %d", h.Base, oldest.Revision, res.Revision, r.tape.current)
	}
	if got := replay(kvs, res.Revision)["/registry/configmaps/shop/a"]; string(got.Value) != "7" {
		t.Fatalf("restored: %+v", got)
	}
}

// When the tape is cut past what was shipped (the shipper was away longer
// than the store keeps history), the bucket starts a new base where the
// tape starts rather than keep a journal with a hole.
func TestATapeCutPastTheBucketStartsANewBase(t *testing.T) {
	r := newRig(t)
	r.tape.write(put("/a", "1"))
	r.sync(t)
	r.tape.write(put("/a", "2"))
	r.tape.write(put("/b", "1"))
	r.tape.write(put("/a", "3"))
	r.tape.from = 4
	res := r.sync(t)
	if res.Base != 4 || res.Shipped != 5 {
		t.Fatalf("after the cut: %+v", res)
	}
	if _, err := WriteTapeFile(context.Background(), r.bucket, []age.Identity{r.id}, Target{Revision: 3}, io.Discard); err == nil {
		t.Fatal("restored across the hole")
	}
	_, raw := r.restore(t, Target{Revision: 5})
	_, kvs := readTapeFile(t, raw)
	if fmt.Sprint(replay(kvs, 5)) != fmt.Sprint(r.tape.stateAt(5)) {
		t.Fatalf("restored %v", replay(kvs, 5))
	}
}

// Two shippers for a while (a change of leader) write overlapping
// segments; the restore reads each revision once.
func TestOverlappingSegmentsRestoreOnce(t *testing.T) {
	r := newRig(t)
	r.tape.write(put("/a", "1"))
	r.sync(t)
	r.tape.write(put("/a", "2"))
	r.tape.write(put("/b", "1"))
	r.sync(t)
	// A second shipper that missed the last pass ships (2, 4] again, and more.
	raw, err := encode([]tape.KV{r.tape.versions[1], r.tape.versions[2]}, r.ship.Recipients)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.bucket.Put(context.Background(), segmentName(2, 4, 0, true), raw); err != nil {
		t.Fatal(err)
	}
	r.tape.write(put("/a", "3"))
	r.sync(t)
	_, file := r.restore(t, Target{})
	_, kvs := readTapeFile(t, file)
	seen := map[string]bool{}
	for _, kv := range kvs {
		k := kv.Key + "@" + strconv.FormatInt(kv.ModRevision, 10)
		if seen[k] {
			t.Fatalf("%s restored twice", k)
		}
		seen[k] = true
	}
	if fmt.Sprint(replay(kvs, 5)) != fmt.Sprint(r.tape.stateAt(5)) {
		t.Fatalf("restored %v", replay(kvs, 5))
	}
}

func TestParseBucket(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "a")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "b")
	b, err := ParseBucket("s3://tapes/a/b/", "", "eu-central-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "tapes" || b.Prefix != "a/b" || b.Endpoint.Host != "s3.eu-central-1.amazonaws.com" || b.String() != "s3://tapes/a/b" {
		t.Fatalf("%+v", b)
	}
	for _, bad := range []string{"tapes/a", "https://x/tapes", "s3:///a"} {
		if _, err := ParseBucket(bad, "", ""); err == nil {
			t.Fatalf("%s parsed", bad)
		}
	}
	if q := canonicalQuery(url.Values{"prefix": {"a/b c"}, "list-type": {"2"}}); q != "list-type=2&prefix=a%2Fb%20c" {
		t.Fatalf("query %s", q)
	}
}
