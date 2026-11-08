// Package replica keeps a copy of the cluster's tape in S3-compatible
// object storage, so that the tape, and the store with it, outlive the
// cluster: a base (every key at a revision) and the journal after it,
// shipped as it is written, and a tape file made back from them up to any
// revision or moment, which kuberoot-store tape restore turns into a store.
//
// Under the bucket's prefix:
//
//	base/<revision>-<time>.jsonl.gz[.age]           the keys at revision
//	journal/<after>-<upto>-<time>.jsonl.gz[.age]    the versions in (after, upto]
//
// revisions zero-padded to 20 digits, times in Unix milliseconds (the base's
// revision's, the last version's). Each object is JSON lines of versions as
// the tape API returns them, gzipped, and encrypted with age when it ends
// in .age. The values are what the API server stored: Secrets are as
// encrypted as the API server keeps them, and the rest is in the clear
// unless age encrypts it.
package replica

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"filippo.io/age"

	"github.com/tym83/gachinetes/pkg/tape"
)

// TapeFileFormat is kuberoot's tape file, which kuberoot-store tape restore
// reads: a header line, the base's keys, the journal.
const TapeFileFormat = "kuberoot-tape/v1"

// TapeFileHeader is the first line of a tape file.
type TapeFileHeader struct {
	Format string `json:"format"`
	Base   int64  `json:"base"`
	Time   int64  `json:"time,omitempty"`
}

const (
	baseDir    = "base/"
	journalDir = "journal/"
	ext        = ".jsonl.gz"
	ageExt     = ".age"
)

// Base is a base in the bucket.
type Base struct {
	Name      string
	Revision  int64
	Time      int64
	Encrypted bool
}

// Segment is a part of the journal in the bucket: the versions in
// (After, Upto]; Time is the last one's.
type Segment struct {
	Name      string
	After     int64
	Upto      int64
	Time      int64
	Encrypted bool
}

func pad(n int64) string { return fmt.Sprintf("%020d", n) }

func baseName(rev, at int64, encrypted bool) string {
	n := baseDir + pad(rev) + "-" + strconv.FormatInt(at, 10) + ext
	if encrypted {
		n += ageExt
	}
	return n
}

func segmentName(after, upto, at int64, encrypted bool) string {
	n := journalDir + pad(after) + "-" + pad(upto) + "-" + strconv.FormatInt(at, 10) + ext
	if encrypted {
		n += ageExt
	}
	return n
}

// parseName splits a name of the bucket into its numbers; ok is false for
// what is not a base or a segment.
func parseName(name, dir string, n int) (nums []int64, encrypted, ok bool) {
	if !strings.HasPrefix(name, dir) {
		return nil, false, false
	}
	rest := strings.TrimPrefix(name, dir)
	if strings.HasSuffix(rest, ageExt) {
		rest, encrypted = strings.TrimSuffix(rest, ageExt), true
	}
	if !strings.HasSuffix(rest, ext) {
		return nil, false, false
	}
	parts := strings.Split(strings.TrimSuffix(rest, ext), "-")
	if len(parts) != n {
		return nil, false, false
	}
	for _, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, false, false
		}
		nums = append(nums, v)
	}
	return nums, encrypted, true
}

// Catalog is what the bucket holds: its bases and segments, in order.
type Catalog struct {
	Bases    []Base
	Segments []Segment
}

func catalogOf(objs []Object) Catalog {
	var c Catalog
	for _, o := range objs {
		if n, enc, ok := parseName(o.Name, baseDir, 2); ok {
			c.Bases = append(c.Bases, Base{Name: o.Name, Revision: n[0], Time: n[1], Encrypted: enc})
		}
		if n, enc, ok := parseName(o.Name, journalDir, 3); ok && n[1] > n[0] {
			c.Segments = append(c.Segments, Segment{Name: o.Name, After: n[0], Upto: n[1], Time: n[2], Encrypted: enc})
		}
	}
	return c
}

// Shipped is the newest revision the bucket holds a contiguous journal to,
// from its newest base; 0 when it holds no base.
func (c Catalog) Shipped() int64 {
	if len(c.Bases) == 0 {
		return 0
	}
	newest := c.Bases[len(c.Bases)-1]
	return c.chainFrom(newest.Revision).end
}

type chain struct {
	segments []Segment
	end      int64
}

// chainFrom is the journal from rev on, as far as it goes without a gap:
// at each point, the segment that starts at or before it and reaches
// furthest. Segments may overlap, after a change of shipper.
func (c Catalog) chainFrom(rev int64) chain {
	ch := chain{end: rev}
	for {
		best := -1
		for i, s := range c.Segments {
			if s.After <= ch.end && s.Upto > ch.end && (best < 0 || s.Upto > c.Segments[best].Upto) {
				best = i
			}
		}
		if best < 0 {
			return ch
		}
		ch.segments = append(ch.segments, c.Segments[best])
		ch.end = c.Segments[best].Upto
	}
}

// encode is versions as an object: JSON lines, gzipped, encrypted to the
// recipients when there are any.
func encode(kvs []tape.KV, recipients []age.Recipient) ([]byte, error) {
	var buf bytes.Buffer
	var sink io.Writer = &buf
	var sealed io.WriteCloser
	if len(recipients) > 0 {
		var err error
		if sealed, err = age.Encrypt(&buf, recipients...); err != nil {
			return nil, err
		}
		sink = sealed
	}
	zw := gzip.NewWriter(sink)
	enc := json.NewEncoder(zw)
	for _, kv := range kvs {
		if err := enc.Encode(kv); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if sealed != nil {
		if err := sealed.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// decode reads an object's versions back.
func decode(raw []byte, encrypted bool, identities []age.Identity) ([]tape.KV, error) {
	var src io.Reader = bytes.NewReader(raw)
	if encrypted {
		if len(identities) == 0 {
			return nil, fmt.Errorf("encrypted with age: an identity is needed")
		}
		r, err := age.Decrypt(src, identities...)
		if err != nil {
			return nil, err
		}
		src = r
	}
	zr, err := gzip.NewReader(src)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var out []tape.KV
	for sc.Scan() {
		var kv tape.KV
		if err := json.Unmarshal(sc.Bytes(), &kv); err != nil {
			return nil, err
		}
		out = append(out, kv)
	}
	return out, sc.Err()
}
