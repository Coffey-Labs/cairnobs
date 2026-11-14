package state

// Leases: one copy of obs-beacond does a job at a time (docs/scale-and-
// availability.md, One evaluator at a time). The holder renews before the
// lease runs out; when it doesn't, another copy takes it over and the epoch
// goes up. Writes that only the holder may make carry its Fence, and the
// database refuses them from anyone whose epoch has passed: a copy that was
// paused and wakes after losing the lease can't write.

import (
	"database/sql"
	"errors"
	"time"
)

const leasesSchema = `
CREATE TABLE IF NOT EXISTS leases (
	name TEXT PRIMARY KEY, holder TEXT NOT NULL, epoch BIGINT NOT NULL, until_ms BIGINT NOT NULL
);
`

// Fence names a lease as one holder holds it.
type Fence struct {
	Lease, Holder string
	Epoch         int64
}

// ErrFenced: the lease has passed to someone else; the write wasn't made.
var ErrFenced = errors.New("the lease has passed to another copy")

// Acquire takes the lease, or renews it for its holder, until now+ttl. It
// says whether holder has it, and at which epoch: the epoch stays the same
// while one holder renews and goes up when another takes over.
func (d *DB) Acquire(lease, holder string, ttl time.Duration, now time.Time) (Fence, bool, error) {
	var epoch int64
	err := d.queryRow(`INSERT INTO leases (name, holder, epoch, until_ms) VALUES (?, ?, 1, ?)
		ON CONFLICT (name) DO UPDATE SET
			epoch = CASE WHEN leases.holder = excluded.holder THEN leases.epoch ELSE leases.epoch + 1 END,
			holder = excluded.holder, until_ms = excluded.until_ms
		WHERE leases.holder = excluded.holder OR leases.until_ms < ?
		RETURNING epoch`, lease, holder, now.Add(ttl).UnixMilli(), now.UnixMilli()).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Fence{}, false, nil
	}
	if err != nil {
		return Fence{}, false, err
	}
	return Fence{Lease: lease, Holder: holder, Epoch: epoch}, true, nil
}

// Release gives the lease up at once (on the way out), so another copy
// needn't wait for it to run out.
func (d *DB) Release(f Fence) error {
	_, err := d.exec(`UPDATE leases SET until_ms = 0 WHERE name = ? AND holder = ? AND epoch = ?`, f.Lease, f.Holder, f.Epoch)
	return err
}

// LeaseHolder says who holds a lease now; "" when nobody does.
func (d *DB) LeaseHolder(lease string, now time.Time) (string, error) {
	var h string
	err := d.queryRow(`SELECT holder FROM leases WHERE name = ? AND until_ms >= ?`, lease, now.UnixMilli()).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return h, err
}

// fenced is the condition a fenced write adds: the fence still holds.
const fenced = `EXISTS (SELECT 1 FROM leases WHERE name = ? AND holder = ? AND epoch = ?)`

func (f *Fence) args() []any { return []any{f.Lease, f.Holder, f.Epoch} }
