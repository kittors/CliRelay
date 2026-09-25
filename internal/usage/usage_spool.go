package usage

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// usageSpool is an append-only, on-disk FIFO of request log writes the
// database could not take. Records go to numbered segment files (JSON lines,
// mode 0600, directory 0700 because they carry API keys and, with body
// storage on, request bodies). The replayer reads segments oldest first and
// deletes one only after every record in it has been committed or found
// already committed, so a crash at any point loses nothing that was appended
// and at worst replays records the idempotency keys then skip.
//
// Appends reach the page cache immediately and are fsynced in batches by a
// background flusher, so a process crash loses nothing and a power loss at
// most the last flush interval.
type usageSpool struct {
	dir             string
	maxBytes        int64
	segmentMaxBytes int64

	mu         sync.Mutex
	segments   []*usageSpoolSegment // oldest first; the last may be active
	active     *os.File
	activeSeg  *usageSpoolSegment
	nextSeq    uint64
	totalBytes int64
	dirty      bool
	closed     bool
	// leased is the segment the replayer is reading. Space reclaim skips it so
	// a record is never deleted halfway through its replay.
	leased uint64

	// wake nudges the replayer after an append.
	wake      chan struct{}
	stopFlush chan struct{}
	flushDone chan struct{}

	appended       atomic.Int64
	replayed       atomic.Int64
	duplicates     atomic.Int64
	droppedRecords atomic.Int64
	rejected       atomic.Int64
	corrupt        atomic.Int64
}

type usageSpoolSegment struct {
	seq     uint64
	path    string
	bytes   int64
	records int64
}

const (
	usageSpoolSegmentPrefix      = "usage-"
	usageSpoolSegmentSuffix      = ".jsonl"
	usageSpoolMaxSegmentBytes    = 8 << 20
	usageSpoolFlushInterval      = 200 * time.Millisecond
	usageSpoolReadBufferBytes    = 64 << 10
	usageSpoolDefaultMaxBytes    = 1 << 30
	usageSpoolMinSegmentFraction = 8
)

var (
	errUsageSpoolClosed = errors.New("usage: spool is closed")
	errUsageSpoolFull   = errors.New("usage: spool is full")
)

func usageSpoolSegmentName(seq uint64) string {
	return fmt.Sprintf("%s%020d%s", usageSpoolSegmentPrefix, seq, usageSpoolSegmentSuffix)
}

func parseUsageSpoolSegmentName(name string) (uint64, bool) {
	if !strings.HasPrefix(name, usageSpoolSegmentPrefix) || !strings.HasSuffix(name, usageSpoolSegmentSuffix) {
		return 0, false
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(name, usageSpoolSegmentPrefix), usageSpoolSegmentSuffix)
	seq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || seq == 0 {
		return 0, false
	}
	return seq, true
}

// openUsageSpool opens (or creates) the spool in dir and adopts the segments a
// previous process left behind.
func openUsageSpool(dir string, maxBytes int64) (*usageSpool, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("usage: spool directory is empty")
	}
	if maxBytes <= 0 {
		maxBytes = usageSpoolDefaultMaxBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("usage: create spool directory: %w", err)
	}
	// An operator-provided directory keeps its mode (it may be shared); the
	// segment files are 0600 regardless, only their names would be visible.
	if info, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("usage: stat spool directory: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("usage: spool path %s is not a directory", dir)
	} else if info.Mode().Perm()&0o077 != 0 {
		log.Warnf("usage: spool directory %s is accessible to other users (mode %o); its records carry API keys, 0700 is recommended", dir, info.Mode().Perm())
	}
	segmentMax := min(max(maxBytes/usageSpoolMinSegmentFraction, 1), int64(usageSpoolMaxSegmentBytes))
	s := &usageSpool{
		dir:             dir,
		maxBytes:        maxBytes,
		segmentMaxBytes: segmentMax,
		nextSeq:         1,
		wake:            make(chan struct{}, 1),
		stopFlush:       make(chan struct{}),
		flushDone:       make(chan struct{}),
	}
	if err := s.adoptExistingSegments(); err != nil {
		return nil, err
	}
	go s.flushLoop()
	return s, nil
}

func (s *usageSpool) adoptExistingSegments() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("usage: read spool directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		seq, ok := parseUsageSpoolSegmentName(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("usage: stat spool segment: %w", err)
		}
		if info.Size() == 0 {
			_ = os.Remove(path)
			continue
		}
		_ = os.Chmod(path, 0o600)
		records, err := countUsageSpoolRecords(path)
		if err != nil {
			return err
		}
		s.segments = append(s.segments, &usageSpoolSegment{seq: seq, path: path, bytes: info.Size(), records: records})
		s.totalBytes += info.Size()
		if seq >= s.nextSeq {
			s.nextSeq = seq + 1
		}
	}
	sort.Slice(s.segments, func(i, j int) bool { return s.segments[i].seq < s.segments[j].seq })
	if len(s.segments) > 0 {
		log.Warnf("usage: spool %s holds %d request log records (%d bytes) from an earlier run; they are replayed once the database accepts writes",
			s.dir, s.pendingRecordsLocked(), s.totalBytes)
	}
	return nil
}

// countUsageSpoolRecords counts lines, including a torn last line.
func countUsageSpoolRecords(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("usage: open spool segment: %w", err)
	}
	defer f.Close()
	buf := make([]byte, usageSpoolReadBufferBytes)
	var count int64
	var last byte
	for {
		n, err := f.Read(buf)
		if n > 0 {
			count += int64(bytes.Count(buf[:n], []byte{'\n'}))
			last = buf[n-1]
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("usage: read spool segment: %w", err)
		}
	}
	if last != 0 && last != '\n' {
		count++
	}
	return count, nil
}

// append adds one newline-terminated record. When the spool is at its cap it
// deletes the oldest segments to make room, which is logged as data loss.
func (s *usageSpool) append(line []byte) error {
	size := int64(len(line))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errUsageSpoolClosed
	}
	if size > s.maxBytes {
		return fmt.Errorf("%w: record of %d bytes exceeds the %d byte cap", errUsageSpoolFull, size, s.maxBytes)
	}
	for s.totalBytes+size > s.maxBytes {
		if !s.dropOldestLocked() {
			return fmt.Errorf("%w: %d bytes pending, nothing left to reclaim", errUsageSpoolFull, s.totalBytes)
		}
	}
	if s.active == nil || (s.activeSeg.bytes > 0 && s.activeSeg.bytes+size > s.segmentMaxBytes) {
		if err := s.rotateLocked(); err != nil {
			return err
		}
	}
	written, err := s.active.Write(line)
	s.activeSeg.bytes += int64(written)
	s.totalBytes += int64(written)
	if err != nil {
		// A short write leaves a torn line at the end of the segment. Seal it so
		// the next record starts on a fresh file; replay skips the torn line.
		s.sealActiveLocked()
		return fmt.Errorf("usage: write spool segment: %w", err)
	}
	s.activeSeg.records++
	s.dirty = true
	s.appended.Add(1)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// dropOldestLocked frees space by deleting the oldest segment that is neither
// being appended to (it holds the newest records) nor being replayed.
func (s *usageSpool) dropOldestLocked() bool {
	for i, seg := range s.segments {
		if seg == s.activeSeg || seg.seq == s.leased {
			continue
		}
		if err := os.Remove(seg.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Errorf("usage: spool is full and the oldest segment %s cannot be removed: %v", seg.path, err)
			return false
		}
		s.segments = append(s.segments[:i], s.segments[i+1:]...)
		s.totalBytes -= seg.bytes
		s.droppedRecords.Add(seg.records)
		log.Errorf("usage: spool %s reached its %d byte cap; dropped the oldest %d request log records (%s) to make room — their usage is lost",
			s.dir, s.maxBytes, seg.records, filepath.Base(seg.path))
		return true
	}
	return false
}

func (s *usageSpool) rotateLocked() error {
	s.sealActiveLocked()
	seq := s.nextSeq
	path := filepath.Join(s.dir, usageSpoolSegmentName(seq))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("usage: create spool segment: %w", err)
	}
	s.nextSeq++
	syncUsageSpoolDir(s.dir)
	seg := &usageSpoolSegment{seq: seq, path: path}
	s.active, s.activeSeg = f, seg
	s.segments = append(s.segments, seg)
	return nil
}

// sealActiveLocked syncs and closes the segment being appended to. The next
// append starts a new one; the sealed file becomes readable by the replayer.
func (s *usageSpool) sealActiveLocked() {
	if s.active == nil {
		return
	}
	if err := s.active.Sync(); err != nil {
		log.Warnf("usage: fsync spool segment %s: %v", s.activeSeg.path, err)
	}
	_ = s.active.Close()
	seg := s.activeSeg
	s.active, s.activeSeg, s.dirty = nil, nil, false
	if seg.bytes == 0 {
		_ = os.Remove(seg.path)
		s.removeSegmentLocked(seg.seq)
	}
}

func (s *usageSpool) removeSegmentLocked(seq uint64) *usageSpoolSegment {
	for i, seg := range s.segments {
		if seg.seq == seq {
			s.segments = append(s.segments[:i], s.segments[i+1:]...)
			return seg
		}
	}
	return nil
}

// leaseOldest hands the oldest segment to the replayer, sealing it first if it
// is still being appended to so new records move to another file.
func (s *usageSpool) leaseOldest() (usageSpoolSegment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.segments) == 0 {
		return usageSpoolSegment{}, false
	}
	if s.segments[0] == s.activeSeg {
		s.sealActiveLocked()
		if len(s.segments) == 0 {
			return usageSpoolSegment{}, false
		}
	}
	seg := s.segments[0]
	s.leased = seg.seq
	return *seg, true
}

func (s *usageSpool) releaseLease() {
	s.mu.Lock()
	s.leased = 0
	s.mu.Unlock()
}

// recordDone marks one record of the leased segment as stored.
func (s *usageSpool) recordDone(seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seg := range s.segments {
		if seg.seq == seq && seg.records > 0 {
			seg.records--
			return
		}
	}
}

// completeLeased deletes a fully replayed segment.
func (s *usageSpool) completeLeased(seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leased = 0
	seg := s.removeSegmentLocked(seq)
	if seg == nil {
		return nil
	}
	s.totalBytes -= seg.bytes
	if err := os.Remove(seg.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("usage: remove replayed spool segment: %w", err)
	}
	return nil
}

func (s *usageSpool) pendingRecordsLocked() int64 {
	var total int64
	for _, seg := range s.segments {
		total += seg.records
	}
	return total
}

// pending reports the records and bytes still waiting to be written.
func (s *usageSpool) pending() (records, bytes int64, segments int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingRecordsLocked(), s.totalBytes, len(s.segments)
}

func (s *usageSpool) flushLoop() {
	defer close(s.flushDone)
	ticker := time.NewTicker(usageSpoolFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopFlush:
			return
		case <-ticker.C:
			s.flush()
		}
	}
}

// flush fsyncs the active segment without holding the append lock, so a slow
// disk delays durability, not the requests appending records.
func (s *usageSpool) flush() {
	s.mu.Lock()
	f, dirty := s.active, s.dirty
	s.dirty = false
	s.mu.Unlock()
	if !dirty || f == nil {
		return
	}
	// A rotation may close f concurrently; it synced the file before closing.
	if err := f.Sync(); err != nil && !errors.Is(err, os.ErrClosed) {
		log.Warnf("usage: fsync spool segment: %v", err)
	}
}

// close seals the active segment and stops the flusher. Pending segments stay
// on disk for the next process.
func (s *usageSpool) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.sealActiveLocked()
	s.mu.Unlock()
	close(s.stopFlush)
	<-s.flushDone
}

// readUsageSpoolSegment calls fn for every line from offset on. fn receives
// the line without its newline, whether the line was complete, and the offset
// just past it; it returns false to stop.
func readUsageSpoolSegment(path string, offset int64, fn func(line []byte, complete bool, next int64) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("usage: open spool segment: %w", err)
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return fmt.Errorf("usage: seek spool segment: %w", err)
		}
	}
	reader := bufio.NewReaderSize(f, usageSpoolReadBufferBytes)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			offset += int64(len(line))
			complete := line[len(line)-1] == '\n'
			if complete {
				line = line[:len(line)-1]
			}
			if !fn(line, complete, offset) {
				return nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("usage: read spool segment: %w", readErr)
		}
	}
}

// syncUsageSpoolDir persists a new directory entry. Failure only weakens
// durability after a power loss, so it is not reported.
func syncUsageSpoolDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
