// Package tape reads the cluster's history from the kuberoot resource
// store's tape (/tape/v1) and decodes the objects the API server stored.
package tape

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// KV is a version as the store's tape returns it.
type KV struct {
	Key            string `json:"key"`
	Value          []byte `json:"value,omitempty"`
	CreateRevision int64  `json:"createRevision,omitempty"`
	ModRevision    int64  `json:"modRevision"`
	Version        int64  `json:"version,omitempty"`
	Lease          int64  `json:"lease,omitempty"`
	Deleted        bool   `json:"deleted,omitempty"`
	Time           int64  `json:"time,omitempty"`
}

// Journal is a page of the store's journal: the versions written in
// (After, Upto], in revision order.
type Journal struct {
	After int64 `json:"after"`
	Upto  int64 `json:"upto"`
	More  bool  `json:"more,omitempty"`
	KVs   []KV  `json:"kvs"`
}

// ErrOutOfTape is a read of a revision the tape does not hold: before it
// starts, or after the current one.
var ErrOutOfTape = errors.New("the revision is not on the tape")

// Range is the store's answer to a range read.
type Range struct {
	Revision int64 `json:"revision"`
	Time     int64 `json:"time,omitempty"`
	Count    int64 `json:"count"`
	More     bool  `json:"more,omitempty"`
	KVs      []KV  `json:"kvs"`
}

// Grave is a deleted key.
type Grave struct {
	Key       string `json:"key"`
	DeletedAt int64  `json:"deletedAt"`
	Time      int64  `json:"time,omitempty"`
	Last      *KV    `json:"last,omitempty"`
}

// Info is what the tape holds.
type Info struct {
	Current     int64 `json:"current"`
	Compacted   int64 `json:"compacted"`
	From        int64 `json:"from"`
	Started     bool  `json:"started"`
	FromTime    int64 `json:"fromTime,omitempty"`
	CurrentTime int64 `json:"currentTime,omitempty"`
}

// Store reads the tape of a store member, http://127.0.0.1:2382 on a
// kuberoot control plane booted with kuberoot.store-tape.
type Store struct {
	URL    string
	Client *http.Client
	// Decrypter, when set, opens the values the API server encrypted, so
	// that everything read from the tape is plain.
	Decrypter *Decrypter
}

// open decrypts a value read from the tape; one that does not open stays
// as it was, and decoding it says why.
func (s *Store) open(kv *KV) {
	if s.Decrypter == nil || kv.Deleted {
		return
	}
	if v, err := s.Decrypter.Open(kv.Key, kv.Value); err == nil {
		kv.Value = v
	}
}

// Point is a moment of the tape: a revision, or a time when Rev is 0. Both
// zero is now.
type Point struct {
	Rev int64
	At  time.Time
}

func (p Point) query(q url.Values) {
	switch {
	case p.Rev != 0:
		q.Set("rev", strconv.FormatInt(p.Rev, 10))
	case !p.At.IsZero():
		q.Set("at", p.At.UTC().Format(time.RFC3339))
	}
}

func (s *Store) get(ctx context.Context, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	c := s.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		var msg [512]byte
		n, _ := resp.Body.Read(msg[:])
		return fmt.Errorf("tape %s: %w: %s", path, ErrOutOfTape, msg[:n])
	}
	if resp.StatusCode != http.StatusOK {
		var msg [512]byte
		n, _ := resp.Body.Read(msg[:])
		return fmt.Errorf("tape %s: %s: %s", path, resp.Status, msg[:n])
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// prefixEnd is the end of a range reading every key with prefix.
func prefixEnd(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return "\x00"
}

func (s *Store) Info(ctx context.Context) (Info, error) {
	var i Info
	return i, s.get(ctx, "/tape/v1/info", url.Values{}, &i)
}

// Range reads every key under prefix at p.
func (s *Store) Range(ctx context.Context, prefix string, p Point) (Range, error) {
	q := url.Values{"key": {prefix}, "end": {prefixEnd(prefix)}}
	p.query(q)
	var r Range
	if err := s.get(ctx, "/tape/v1/range", q, &r); err != nil {
		return r, err
	}
	for i := range r.KVs {
		s.open(&r.KVs[i])
	}
	return r, nil
}

// Journal reads the versions written after the revision after, in
// revision order, about limit of them (a page ends with a whole revision).
// Values stay as the store holds them, never decrypted: the journal is for
// copies of the store.
func (s *Store) Journal(ctx context.Context, after, limit int64) (Journal, error) {
	var j Journal
	q := url.Values{"after": {strconv.FormatInt(after, 10)}}
	if limit > 0 {
		q.Set("limit", strconv.FormatInt(limit, 10))
	}
	return j, s.get(ctx, "/tape/v1/journal", q, &j)
}

// RawRange reads every key at rev as the store holds it, never decrypted.
func (s *Store) RawRange(ctx context.Context, rev int64) (Range, error) {
	var r Range
	q := url.Values{"key": {""}, "end": {"\x00"}, "rev": {strconv.FormatInt(rev, 10)}}
	return r, s.get(ctx, "/tape/v1/range", q, &r)
}

// History is every version of key, oldest first.
func (s *Store) History(ctx context.Context, key string) ([]KV, error) {
	var out []KV
	if err := s.get(ctx, "/tape/v1/history", url.Values{"key": {key}}, &out); err != nil {
		return nil, err
	}
	for i := range out {
		s.open(&out[i])
	}
	return out, nil
}

// Graveyard is the keys under prefix deleted and not written since.
func (s *Store) Graveyard(ctx context.Context, prefix string) ([]Grave, error) {
	var out []Grave
	if err := s.get(ctx, "/tape/v1/graveyard", url.Values{"key": {prefix}, "end": {prefixEnd(prefix)}}, &out); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Last != nil {
			s.open(out[i].Last)
		}
	}
	return out, nil
}
