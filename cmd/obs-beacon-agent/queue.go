package main

import (
	"cmp"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The queue holds reports obs-beacond hasn't taken yet, oldest first: at
// most maxQueue of them (an hour at the default interval) and maxQueueBytes
// in all. The oldest go first when it fills.
const (
	maxQueue      = 240
	maxQueueBytes = 64 << 20
)

// queue keeps a report in memory until a send fails, then on disk under
// state_dir/queue, one file per report, so an agent restart (an upgrade, a
// reboot) while obs-beacond is away loses nothing. A report taken at the
// first try never touches the disk.
type queue struct {
	dir    string // "": memory only
	items  []queued
	bytes  int64
	last   int64 // the newest id given out, so ids stay unique and in order
	warned bool  // the disk problem is logged once
}

type queued struct {
	id     int64 // the report's time in Unix nanoseconds, and its file name
	size   int64
	body   []byte // nil once it's on disk
	onDisk bool
}

func (q *queue) file(id int64) string { return filepath.Join(q.dir, fmt.Sprintf("%019d.json", id)) }

// openQueue picks up what an earlier run left in dir. Reports older than
// maxAge (what a full queue spans) are dropped: they'd be history nobody
// asked for. A dir that can't be used leaves the queue in memory only.
func openQueue(dir string, maxAge time.Duration, now time.Time) *queue {
	q := &queue{dir: dir}
	if dir == "" {
		return q
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("queue: %v; keeping unsent reports in memory only", err)
		q.dir = ""
		return q
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("queue: %v; keeping unsent reports in memory only", err)
		q.dir = ""
		return q
	}
	cutoff := now.Add(-maxAge).UnixNano()
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") { // a write cut short
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSuffix(name, ".json"), 10, 64)
		if err != nil || !strings.HasSuffix(name, ".json") || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if id < cutoff {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		q.items = append(q.items, queued{id: id, size: info.Size(), onDisk: true})
		q.bytes += info.Size()
		q.last = max(q.last, id)
	}
	slices.SortFunc(q.items, func(a, b queued) int { return cmp.Compare(a.id, b.id) })
	q.trim()
	return q
}

func (q *queue) len() int { return len(q.items) }

// push adds a report collected at t.
func (q *queue) push(t time.Time, b []byte) {
	id := t.UnixNano()
	if id <= q.last {
		id = q.last + 1
	}
	q.last = id
	q.items = append(q.items, queued{id: id, size: int64(len(b)), body: b})
	q.bytes += int64(len(b))
	q.trim()
}

// trim drops the oldest past either limit, keeping at least the newest.
func (q *queue) trim() {
	for len(q.items) > 1 && (len(q.items) > maxQueue || q.bytes > maxQueueBytes) {
		q.pop()
	}
}

// peek is the oldest report.
func (q *queue) peek() ([]byte, error) {
	it := q.items[0]
	if !it.onDisk {
		return it.body, nil
	}
	return os.ReadFile(q.file(it.id))
}

// pop removes the oldest report.
func (q *queue) pop() {
	it := q.items[0]
	if it.onDisk {
		_ = os.Remove(q.file(it.id))
	}
	q.bytes -= it.size
	q.items[0] = queued{}
	q.items = q.items[1:]
}

// persist writes what's only in memory to disk. A write that fails leaves
// the report in memory, where it still goes out once obs-beacond is back.
func (q *queue) persist() {
	if q.dir == "" {
		return
	}
	for i := range q.items {
		it := &q.items[i]
		if it.onDisk {
			continue
		}
		if err := writeQueued(q.dir, q.file(it.id), it.body); err != nil {
			if !q.warned {
				q.warned = true
				log.Printf("queue: can't keep unsent reports on disk, keeping them in memory: %v", err)
			}
			return
		}
		it.onDisk, it.body = true, nil
	}
	q.warned = false
}

func writeQueued(dir, p string, b []byte) error {
	tmp, err := os.CreateTemp(dir, ".report-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
