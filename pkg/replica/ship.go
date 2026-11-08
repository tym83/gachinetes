package replica

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"filippo.io/age"

	"github.com/tym83/gachinetes/pkg/tape"
)

// Shipper ships the tape of a store member to a bucket as it is written.
type Shipper struct {
	Tape   *tape.Store
	Bucket *Bucket
	// Recipients the objects are encrypted to; none ships them in the
	// clear.
	Recipients []age.Recipient
	// Page is about how many versions go to one segment; 0 is 5000.
	Page int64
	// BaseEvery is how often a new base is written, so that a restore
	// need not replay the journal from the first one, and retention can
	// drop what is before it; 0 is a day.
	BaseEvery time.Duration
	// Retention is how long the bucket keeps the tape: older bases and
	// the segments before them go, as long as a base at least that old
	// remains. 0 keeps everything.
	Retention time.Duration
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

func (s *Shipper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Shipper) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// SyncResult is what a pass of Sync shipped.
type SyncResult struct {
	Base     int64 // the revision of a base written, 0 for none
	Segments int
	Versions int
	Shipped  int64 // the newest revision the bucket holds now
	Pruned   int   // objects retention removed
}

// Sync ships what the bucket does not hold yet: a base when it has none or
// the journal it holds cannot go on (the tape was cut past it), then the
// journal up to the current revision, a new base when the newest is older
// than BaseEvery, and drops what retention no longer keeps.
func (s *Shipper) Sync(ctx context.Context) (SyncResult, error) {
	var res SyncResult
	objs, err := s.Bucket.List(ctx, "")
	if err != nil {
		return res, err
	}
	cat := catalogOf(objs)
	info, err := s.Tape.Info(ctx)
	if err != nil {
		return res, err
	}
	shipped := cat.Shipped()
	if len(cat.Bases) == 0 || shipped < info.From || shipped > info.Current {
		// Nothing yet; or the tape no longer holds what follows the
		// journal shipped, or holds less than it (a store restored from
		// further back): a new base where the tape starts.
		if len(cat.Bases) > 0 {
			s.logf("tape replica: the bucket's journal ends at %d, the tape holds %d to %d: a new base", shipped, info.From, info.Current)
		}
		b, err := s.writeBase(ctx, info.From)
		if err != nil {
			return res, err
		}
		cat.Bases = append(cat.Bases, b)
		res.Base, shipped = b.Revision, b.Revision
	}
	page := s.Page
	if page == 0 {
		page = 5000
	}
	for {
		j, err := s.Tape.Journal(ctx, shipped, page)
		if errors.Is(err, tape.ErrOutOfTape) {
			// Cut while shipping: the next pass starts a new base.
			return res, fmt.Errorf("the tape no longer holds revision %d: %w", shipped, err)
		}
		if err != nil {
			return res, err
		}
		if len(j.KVs) > 0 {
			last := j.KVs[len(j.KVs)-1]
			upto := last.ModRevision
			raw, err := encode(j.KVs, s.Recipients)
			if err != nil {
				return res, err
			}
			seg := Segment{Name: segmentName(shipped, upto, last.Time, len(s.Recipients) > 0), After: shipped, Upto: upto, Time: last.Time}
			if err := s.Bucket.Put(ctx, seg.Name, raw); err != nil {
				return res, err
			}
			cat.Segments = append(cat.Segments, seg)
			res.Segments++
			res.Versions += len(j.KVs)
			shipped = upto
		}
		if !j.More {
			break
		}
	}
	res.Shipped = shipped
	newest := cat.Bases[len(cat.Bases)-1]
	every := s.BaseEvery
	if every == 0 {
		every = 24 * time.Hour
	}
	if shipped > newest.Revision && newest.Time != 0 && s.now().Sub(time.UnixMilli(newest.Time)) >= every {
		b, err := s.writeBase(ctx, shipped)
		if err != nil {
			return res, err
		}
		cat.Bases = append(cat.Bases, b)
		res.Base = b.Revision
	}
	if s.Retention > 0 {
		n, err := s.prune(ctx, cat)
		res.Pruned = n
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// writeBase ships every key at rev.
func (s *Shipper) writeBase(ctx context.Context, rev int64) (Base, error) {
	r, err := s.Tape.RawRange(ctx, rev)
	if err != nil {
		return Base{}, fmt.Errorf("base at %d: %w", rev, err)
	}
	at := r.Time
	if at == 0 {
		// A revision from before the store kept times: the base is
		// stamped when it was read, which is no earlier than the truth.
		at = s.now().UnixMilli()
	}
	raw, err := encode(r.KVs, s.Recipients)
	if err != nil {
		return Base{}, err
	}
	b := Base{Name: baseName(rev, at, len(s.Recipients) > 0), Revision: rev, Time: at, Encrypted: len(s.Recipients) > 0}
	return b, s.Bucket.Put(ctx, b.Name, raw)
}

// prune drops the bases older than the newest one that is at least
// Retention old, and the segments that end before it: the bucket keeps the
// tape for Retention and a little more, never less.
func (s *Shipper) prune(ctx context.Context, cat Catalog) (int, error) {
	cut := s.now().Add(-s.Retention).UnixMilli()
	keep := -1
	for i, b := range cat.Bases {
		if b.Time <= cut {
			keep = i
		}
	}
	if keep <= 0 {
		return 0, nil
	}
	from := cat.Bases[keep].Revision
	n := 0
	for _, b := range cat.Bases[:keep] {
		if err := s.Bucket.Delete(ctx, b.Name); err != nil {
			return n, err
		}
		n++
	}
	for _, seg := range cat.Segments {
		if seg.Upto <= from {
			if err := s.Bucket.Delete(ctx, seg.Name); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// Run ships every interval until ctx ends; a pass that fails is tried
// again at the next.
func (s *Shipper) Run(ctx context.Context, interval time.Duration) {
	for {
		res, err := s.Sync(ctx)
		switch {
		case err != nil:
			s.logf("tape replica: %v", err)
		case res.Versions > 0 || res.Base != 0 || res.Pruned > 0:
			s.logf("tape replica: shipped %d versions in %d segments to %s, up to %d (base %d, pruned %d)",
				res.Versions, res.Segments, s.Bucket, res.Shipped, res.Base, res.Pruned)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
