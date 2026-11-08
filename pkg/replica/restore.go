package replica

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"filippo.io/age"
)

// Target is where a restore stops: a revision, or a moment when Revision
// is 0; both zero is all the bucket holds.
type Target struct {
	Revision int64
	At       time.Time
}

// Restored is what a tape file made from the bucket holds.
type Restored struct {
	Base     int64 // the base's revision: the tape reads from here
	BaseTime int64
	Revision int64 // the newest revision in it
	Time     int64 // when that was written, Unix milliseconds
	Versions int
}

// Catalog lists what the bucket holds.
func (b *Bucket) Catalog(ctx context.Context) (Catalog, error) {
	objs, err := b.List(ctx, "")
	if err != nil {
		return Catalog{}, err
	}
	return catalogOf(objs), nil
}

// pick is the base to restore from and the journal after it: the oldest
// base whose journal reaches the target without a gap, so that the store
// comes back with as much of its history as the bucket keeps.
func (c Catalog) pick(t Target) (Base, chain, error) {
	if len(c.Bases) == 0 {
		return Base{}, chain{}, fmt.Errorf("the bucket holds no base of the tape")
	}
	for _, b := range c.Bases {
		ch := c.chainFrom(b.Revision)
		switch {
		case t.Revision != 0:
			if b.Revision <= t.Revision && t.Revision <= ch.end {
				return b, ch, nil
			}
		case !t.At.IsZero():
			if b.Time <= t.At.UnixMilli() {
				return b, ch, nil
			}
		default:
			if ch.end >= c.Shipped() {
				return b, ch, nil
			}
		}
	}
	switch {
	case t.Revision != 0:
		return Base{}, chain{}, fmt.Errorf("the bucket holds no unbroken tape to revision %d (it ships to %d)", t.Revision, c.Shipped())
	case !t.At.IsZero():
		return Base{}, chain{}, fmt.Errorf("the bucket's tape starts after %s", t.At.UTC().Format(time.RFC3339))
	}
	return Base{}, chain{}, fmt.Errorf("the bucket holds no unbroken tape")
}

// WriteTapeFile writes the tape from the bucket, up to the target, as a
// tape file for kuberoot-store tape restore.
func WriteTapeFile(ctx context.Context, b *Bucket, identities []age.Identity, t Target, w io.Writer) (Restored, error) {
	var res Restored
	cat, err := b.Catalog(ctx)
	if err != nil {
		return res, err
	}
	base, ch, err := cat.pick(t)
	if err != nil {
		return res, err
	}
	res.Base, res.BaseTime, res.Revision, res.Time = base.Revision, base.Time, base.Revision, base.Time
	enc := json.NewEncoder(w)
	if err := enc.Encode(TapeFileHeader{Format: TapeFileFormat, Base: base.Revision, Time: base.Time}); err != nil {
		return res, err
	}
	raw, err := b.Get(ctx, base.Name)
	if err != nil {
		return res, err
	}
	kvs, err := decode(raw, base.Encrypted, identities)
	if err != nil {
		return res, fmt.Errorf("%s: %w", base.Name, err)
	}
	for _, kv := range kvs {
		if kv.ModRevision > base.Revision || kv.Deleted {
			return res, fmt.Errorf("%s: %s at revision %d is not of the base", base.Name, kv.Key, kv.ModRevision)
		}
		if err := enc.Encode(kv); err != nil {
			return res, err
		}
	}
	at := int64(0)
	if !t.At.IsZero() {
		at = t.At.UnixMilli()
	}
	pos := base.Revision
	for _, seg := range ch.segments {
		raw, err := b.Get(ctx, seg.Name)
		if err != nil {
			return res, err
		}
		kvs, err := decode(raw, seg.Encrypted, identities)
		if err != nil {
			return res, fmt.Errorf("%s: %w", seg.Name, err)
		}
		for _, kv := range kvs {
			if kv.ModRevision <= pos {
				// Shipped twice, by two shippers.
				continue
			}
			if t.Revision != 0 && kv.ModRevision > t.Revision {
				return res, nil
			}
			if at != 0 && kv.Time > at {
				return res, nil
			}
			if err := enc.Encode(kv); err != nil {
				return res, err
			}
			res.Revision, res.Time = kv.ModRevision, kv.Time
			res.Versions++
		}
		pos = max(pos, res.Revision)
	}
	if t.Revision != 0 && res.Revision < t.Revision {
		return res, fmt.Errorf("the bucket's journal ends at revision %d, before %d", res.Revision, t.Revision)
	}
	return res, nil
}
