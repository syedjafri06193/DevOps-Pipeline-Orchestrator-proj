// Package store is the system of record.
//
// The design document specifies SQLite (section 14.2, `modernc.org/sqlite`
// for a cgo-free static binary), and `migrations/0001_init.sql` holds that
// schema verbatim so the swap is mechanical. What is implemented here is a
// small append-only journal with the same transactional guarantees, because
// this build has no module proxy and therefore no driver -- see
// docs/notes-on-the-spec.md.
//
// That constraint turned out to serve the project. The whole document is
// about crash safety:
//
//	"Persist the transition, then do the thing. Write DEPLOYING and commit
//	*before* calling provider.Deploy()."
//
// A store whose durability you implement yourself is a store whose durability
// you can test by truncating the file mid-record, flipping a bit in a payload,
// and reopening. test/chaos does exactly that, which is not something you can
// do to a driver you did not write.
//
// # Format
//
// One file, append-only. Each record is:
//
//	magic    4 bytes   "ORJ1"
//	length   4 bytes   big-endian payload length
//	crc32    4 bytes   IEEE CRC of the payload
//	payload  N bytes   JSON
//
// A record is only real once its bytes and an fsync have landed. Recovery
// reads forward until a record fails to parse, fails its CRC, or is short,
// and stops there -- everything before that point is intact, and the
// truncated tail is discarded. That is the same contract a WAL gives: the
// last transaction may be lost, but nothing before it can be corrupted.
//
// # Transactions
//
// A transaction buffers records and writes them in a single append plus one
// fsync. Either the whole group is durable or -- if the process dies during
// the write -- recovery stops at the first bad record and the group is gone
// in its entirety, because the group commit marker is the last record written.
package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

var (
	magic = [4]byte{'O', 'R', 'J', '1'}

	// ErrCorrupt is returned when a record's bytes are intact but its
	// contents are not parseable, which is a different fault from a torn
	// tail and must not be silently truncated away.
	ErrCorrupt = errors.New("store: journal record is corrupt")
)

const headerSize = 12

// recordKind identifies what a journal record carries. Kept as a short string
// rather than an integer so a journal can be read with `strings` during an
// incident, which is the sort of thing that matters at 3am.
type recordKind string

const (
	kindDeployment   recordKind = "deployment"
	kindStep         recordKind = "step"
	kindLease        recordKind = "lease"
	kindLeaseRelease recordKind = "lease_release"
	kindAudit        recordKind = "audit"
	kindOutbox       recordKind = "outbox"
	kindOutboxState  recordKind = "outbox_state"
	kindHealthSample recordKind = "health"
	kindApproval     recordKind = "approval"
	kindFreeze       recordKind = "freeze"
	kindCommit       recordKind = "commit"
)

type record struct {
	Kind recordKind      `json:"k"`
	Data json.RawMessage `json:"d,omitempty"`
	// Seq is the transaction sequence this record belongs to. Records whose
	// transaction never committed are discarded on recovery.
	Seq uint64 `json:"s"`
}

// journal is the append-only file underneath the store.
type journal struct {
	mu   sync.Mutex
	f    *os.File
	path string
	seq  uint64

	// syncs counts fsyncs, so tests can assert that a commit actually
	// durably committed rather than merely buffered.
	syncs int
	// failWrite lets the chaos tests simulate a disk that stops accepting
	// writes partway through a commit.
	failWrite func(n int) error
	written   int
}

func openJournal(path string) (*journal, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	return &journal{f: f, path: path}, nil
}

// replay reads every committed record in order.
//
// Uncommitted trailing records -- a group whose commit marker never landed --
// are dropped, and the file is truncated back to the last good offset so the
// next append starts from a clean boundary. Leaving the torn bytes in place
// would make every subsequent open re-scan and re-truncate, and would leave
// a file that looks corrupt to anyone inspecting it.
func (j *journal) replay(apply func(rec record) error) error {
	if _, err := j.f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	type pending struct {
		seq  uint64
		recs []record
	}
	var group pending
	var lastGoodOffset int64
	var offset int64

	header := make([]byte, headerSize)
	for {
		n, err := io.ReadFull(j.f, header)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) || n < headerSize {
			break // torn header: the tail is incomplete
		}
		if err != nil {
			return err
		}
		if [4]byte(header[0:4]) != magic {
			// Not a torn write -- a byte was changed inside an otherwise
			// complete file. Refuse rather than silently discarding
			// everything after it.
			return fmt.Errorf("%w: bad magic at offset %d", ErrCorrupt, offset)
		}
		length := binary.BigEndian.Uint32(header[4:8])
		want := binary.BigEndian.Uint32(header[8:12])

		payload := make([]byte, length)
		if _, err := io.ReadFull(j.f, payload); err != nil {
			break // torn payload
		}
		if crc32.ChecksumIEEE(payload) != want {
			return fmt.Errorf("%w: checksum mismatch at offset %d", ErrCorrupt, offset)
		}

		var rec record
		if err := json.Unmarshal(payload, &rec); err != nil {
			return fmt.Errorf("%w: unparseable record at offset %d: %v", ErrCorrupt, offset, err)
		}

		offset += headerSize + int64(length)

		if rec.Kind == kindCommit {
			if rec.Seq != group.seq {
				return fmt.Errorf("%w: commit for sequence %d but %d records are open",
					ErrCorrupt, rec.Seq, group.seq)
			}
			for _, r := range group.recs {
				if err := apply(r); err != nil {
					return err
				}
			}
			group = pending{}
			lastGoodOffset = offset
			if rec.Seq > j.seq {
				j.seq = rec.Seq
			}
			continue
		}

		if group.seq == 0 {
			group.seq = rec.Seq
		}
		group.recs = append(group.recs, rec)
	}

	// Drop any records after the last commit, and make the file match.
	if _, err := j.f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	size, err := j.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if size != lastGoodOffset {
		if err := j.f.Truncate(lastGoodOffset); err != nil {
			return err
		}
	}
	if _, err := j.f.Seek(lastGoodOffset, io.SeekStart); err != nil {
		return err
	}
	return nil
}

// appendGroup writes records plus a commit marker, then fsyncs.
//
// The commit marker last is the whole design: a crash anywhere before it
// leaves a group that replay discards, so a half-written transaction can
// never be observed.
func (j *journal) appendGroup(recs []record) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	j.seq++
	seq := j.seq

	var buf []byte
	for _, r := range recs {
		r.Seq = seq
		b, err := encodeRecord(r)
		if err != nil {
			j.seq--
			return err
		}
		buf = append(buf, b...)
	}
	commit, err := encodeRecord(record{Kind: kindCommit, Seq: seq})
	if err != nil {
		j.seq--
		return err
	}
	buf = append(buf, commit...)

	if j.failWrite != nil {
		if err := j.failWrite(len(buf)); err != nil {
			// Simulated partial write: put down whatever the fault says
			// landed, without the commit marker.
			var partial int
			var pe *partialWriteError
			if errors.As(err, &pe) {
				partial = pe.Bytes
			}
			if partial > len(buf) {
				partial = len(buf)
			}
			if partial > 0 {
				_, _ = j.f.Write(buf[:partial])
				_ = j.f.Sync()
			}
			return err
		}
	}

	if _, err := j.f.Write(buf); err != nil {
		return err
	}
	if err := j.f.Sync(); err != nil {
		return err
	}
	j.syncs++
	j.written += len(buf)
	return nil
}

// partialWriteError is how a chaos test says "the disk accepted this many
// bytes and then stopped".
type partialWriteError struct {
	Bytes int
}

func (e *partialWriteError) Error() string {
	return fmt.Sprintf("simulated partial write after %d bytes", e.Bytes)
}

func encodeRecord(r record) ([]byte, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	out := make([]byte, headerSize+len(payload))
	copy(out[0:4], magic[:])
	binary.BigEndian.PutUint32(out[4:8], uint32(len(payload)))
	binary.BigEndian.PutUint32(out[8:12], crc32.ChecksumIEEE(payload))
	copy(out[headerSize:], payload)
	return out, nil
}

func (j *journal) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}
