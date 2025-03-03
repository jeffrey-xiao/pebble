// Copyright 2012 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

// Package pebble provides an ordered key/value store.
package pebble // import "github.com/petermattis/pebble"

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/petermattis/pebble/internal/arenaskl"
	"github.com/petermattis/pebble/internal/base"
	"github.com/petermattis/pebble/internal/rate"
	"github.com/petermattis/pebble/internal/record"
	"github.com/petermattis/pebble/vfs"
)

const (
	// minTableCacheSize is the minimum size of the table cache.
	minTableCacheSize = 64

	// numNonTableCacheFiles is an approximation for the number of MaxOpenFiles
	// that we don't use for table caches.
	numNonTableCacheFiles = 10
)

var (
	// ErrNotFound is returned when a get operation does not find the requested
	// key.
	ErrNotFound = base.ErrNotFound
	// ErrClosed is returned when an operation is performed on a closed snapshot
	// or DB.
	ErrClosed = errors.New("pebble: closed")
)

type flushable interface {
	newIter(o *IterOptions) internalIterator
	newFlushIter(o *IterOptions, bytesFlushed *uint64) internalIterator
	newRangeDelIter(o *IterOptions) internalIterator
	totalBytes() uint64
	flushed() chan struct{}
	readyForFlush() bool
	logInfo() (num, size uint64)
}

// Reader is a readable key/value store.
//
// It is safe to call Get and NewIter from concurrent goroutines.
type Reader interface {
	// Get gets the value for the given key. It returns ErrNotFound if the DB
	// does not contain the key.
	//
	// The caller should not modify the contents of the returned slice, but
	// it is safe to modify the contents of the argument after Get returns.
	Get(key []byte) (value []byte, err error)

	// NewIter returns an iterator that is unpositioned (Iterator.Valid() will
	// return false). The iterator can be positioned via a call to SeekGE,
	// SeekLT, First or Last.
	NewIter(o *IterOptions) *Iterator

	// Close closes the Reader. It may or may not close any underlying io.Reader
	// or io.Writer, depending on how the DB was created.
	//
	// It is not safe to close a DB until all outstanding iterators are closed.
	// It is valid to call Close multiple times. Other methods should not be
	// called after the DB has been closed.
	Close() error
}

// Writer is a writable key/value store.
//
// Goroutine safety is dependent on the specific implementation.
type Writer interface {
	// Apply the operations contained in the batch to the DB.
	//
	// It is safe to modify the contents of the arguments after Apply returns.
	Apply(batch *Batch, o *WriteOptions) error

	// Delete deletes the value for the given key. Deletes are blind all will
	// succeed even if the given key does not exist.
	//
	// It is safe to modify the contents of the arguments after Delete returns.
	Delete(key []byte, o *WriteOptions) error

	// DeleteRange deletes all of the keys (and values) in the range [start,end)
	// (inclusive on start, exclusive on end).
	//
	// It is safe to modify the contents of the arguments after Delete returns.
	DeleteRange(start, end []byte, o *WriteOptions) error

	// LogData adds the specified to the batch. The data will be written to the
	// WAL, but not added to memtables or sstables. Log data is never indexed,
	// which makes it useful for testing WAL performance.
	//
	// It is safe to modify the contents of the argument after LogData returns.
	LogData(data []byte, opts *WriteOptions) error

	// Merge merges the value for the given key. The details of the merge are
	// dependent upon the configured merge operation.
	//
	// It is safe to modify the contents of the arguments after Merge returns.
	Merge(key, value []byte, o *WriteOptions) error

	// Set sets the value for the given key. It overwrites any previous value
	// for that key; a DB is not a multi-map.
	//
	// It is safe to modify the contents of the arguments after Set returns.
	Set(key, value []byte, o *WriteOptions) error
}

// DB provides a concurrent, persistent ordered key/value store.
//
// A DB's basic operations (Get, Set, Delete) should be self-explanatory. Get
// and Delete will return ErrNotFound if the requested key is not in the store.
// Callers are free to ignore this error.
//
// A DB also allows for iterating over the key/value pairs in key order. If d
// is a DB, the code below prints all key/value pairs whose keys are 'greater
// than or equal to' k:
//
//	iter := d.NewIter(readOptions)
//	for iter.SeekGE(k); iter.Valid(); iter.Next() {
//		fmt.Printf("key=%q value=%q\n", iter.Key(), iter.Value())
//	}
//	return iter.Close()
//
// The Options struct holds the optional parameters for the DB, including a
// Comparer to define a 'less than' relationship over keys. It is always valid
// to pass a nil *Options, which means to use the default parameter values. Any
// zero field of a non-nil *Options also means to use the default value for
// that parameter. Thus, the code below uses a custom Comparer, but the default
// values for every other parameter:
//
//	db := pebble.Open(&Options{
//		Comparer: myComparer,
//	})
type DB struct {
	dirname        string
	walDirname     string
	opts           *Options
	cmp            Compare
	equal          Equal
	merge          Merge
	split          Split
	abbreviatedKey AbbreviatedKey

	dataDir vfs.File
	walDir  vfs.File

	tableCache tableCache
	newIters   tableNewIters

	commit   *commitPipeline
	fileLock io.Closer

	largeBatchThreshold int
	optionsFileNum      uint64

	// readState provides access to the state needed for reading without needing
	// to acquire DB.mu.
	readState struct {
		sync.RWMutex
		val *readState
	}

	logRecycler logRecycler

	closed int32 // updated atomically

	flushLimiter *rate.Limiter

	// TODO(peter): describe exactly what this mutex protects. So far: every
	// field in the struct.
	mu struct {
		sync.Mutex

		nextJobID int

		versions versionSet

		log struct {
			queue   []uint64
			size    uint64
			bytesIn uint64
			*record.LogWriter
		}

		mem struct {
			cond sync.Cond
			// The current mutable memTable.
			mutable *memTable
			// Queue of flushables (the mutable memtable is at end). Elements are
			// added to the end of the slice and removed from the beginning. Once an
			// index is set it is never modified making a fixed slice immutable and
			// safe for concurrent reads.
			queue []flushable
			// True when the memtable is actively been switched. Both mem.mutable and
			// log.LogWriter are invalid while switching is true.
			switching bool
		}

		compact struct {
			cond           sync.Cond
			flushing       bool
			compacting     bool
			pendingOutputs map[uint64]struct{}
			manual         []*manualCompaction
		}

		cleaner struct {
			cond     sync.Cond
			cleaning bool
		}

		// The list of active snapshots.
		snapshots snapshotList
	}
}

var _ Reader = (*DB)(nil)
var _ Writer = (*DB)(nil)

// Get gets the value for the given key. It returns ErrNotFound if the DB does
// not contain the key.
//
// The caller should not modify the contents of the returned slice, but it is
// safe to modify the contents of the argument after Get returns.
func (d *DB) Get(key []byte) ([]byte, error) {
	return d.getInternal(key, nil /* batch */, nil /* snapshot */)
}

func (d *DB) getInternal(key []byte, b *Batch, s *Snapshot) ([]byte, error) {
	if atomic.LoadInt32(&d.closed) != 0 {
		panic(ErrClosed)
	}

	// Grab and reference the current readState. This prevents the underlying
	// files in the associated version from being deleted if there is a current
	// compaction. The readState is unref'd by Iterator.Close().
	readState := d.loadReadState()

	// Determine the seqnum to read at after grabbing the read state (current and
	// memtables) above.
	var seqNum uint64
	if s != nil {
		seqNum = s.seqNum
	} else {
		seqNum = atomic.LoadUint64(&d.mu.versions.visibleSeqNum)
	}

	var buf struct {
		dbi Iterator
		get getIter
	}

	get := &buf.get
	get.cmp = d.cmp
	get.equal = d.equal
	get.newIters = d.newIters
	get.snapshot = seqNum
	get.key = key
	get.batch = b
	get.mem = readState.memtables
	get.l0 = readState.current.files[0]
	get.version = readState.current

	i := &buf.dbi
	i.cmp = d.cmp
	i.equal = d.equal
	i.merge = d.merge
	i.split = d.split
	i.iter = get
	i.readState = readState

	defer i.Close()
	if !i.First() {
		err := i.Error()
		if err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	return i.Value(), nil
}

// Set sets the value for the given key. It overwrites any previous value
// for that key; a DB is not a multi-map.
//
// It is safe to modify the contents of the arguments after Set returns.
func (d *DB) Set(key, value []byte, opts *WriteOptions) error {
	b := newBatch(d)
	defer b.release()
	_ = b.Set(key, value, opts)
	return d.Apply(b, opts)
}

// Delete deletes the value for the given key. Deletes are blind all will
// succeed even if the given key does not exist.
//
// It is safe to modify the contents of the arguments after Delete returns.
func (d *DB) Delete(key []byte, opts *WriteOptions) error {
	b := newBatch(d)
	defer b.release()
	_ = b.Delete(key, opts)
	return d.Apply(b, opts)
}

// DeleteRange deletes all of the keys (and values) in the range [start,end)
// (inclusive on start, exclusive on end).
//
// It is safe to modify the contents of the arguments after DeleteRange
// returns.
func (d *DB) DeleteRange(start, end []byte, opts *WriteOptions) error {
	b := newBatch(d)
	defer b.release()
	_ = b.DeleteRange(start, end, opts)
	return d.Apply(b, opts)
}

// Merge adds an action to the DB that merges the value at key with the new
// value. The details of the merge are dependent upon the configured merge
// operator.
//
// It is safe to modify the contents of the arguments after Merge returns.
func (d *DB) Merge(key, value []byte, opts *WriteOptions) error {
	b := newBatch(d)
	defer b.release()
	_ = b.Merge(key, value, opts)
	return d.Apply(b, opts)
}

// LogData adds the specified to the batch. The data will be written to the
// WAL, but not added to memtables or sstables. Log data is never indexed,
// which makes it useful for testing WAL performance.
//
// It is safe to modify the contents of the argument after LogData returns.
//
// TODO(peter): untested.
func (d *DB) LogData(data []byte, opts *WriteOptions) error {
	b := newBatch(d)
	defer b.release()
	_ = b.LogData(data, opts)
	return d.Apply(b, opts)
}

// Apply the operations contained in the batch to the DB. If the batch is large
// the contents of the batch may be retained by the database. If that occurs
// the batch contents will be cleared preventing the caller from attempting to
// reuse them.
//
// It is safe to modify the contents of the arguments after Apply returns.
func (d *DB) Apply(batch *Batch, opts *WriteOptions) error {
	if atomic.LoadInt32(&d.closed) != 0 {
		panic(ErrClosed)
	}

	sync := opts.GetSync()
	if sync && d.opts.DisableWAL {
		return errors.New("pebble: WAL disabled")
	}

	if int(batch.memTableSize) >= d.largeBatchThreshold {
		batch.flushable = newFlushableBatch(batch, d.opts.Comparer)
	}
	err := d.commit.Commit(batch, sync)
	if err == nil {
		// If this is a large batch, we need to clear the batch contents as the
		// flushable batch may still be present in the flushables queue.
		if batch.flushable != nil {
			batch.storage.data = nil
		}
	}
	return err
}

func (d *DB) commitApply(b *Batch, mem *memTable) error {
	if b.flushable != nil {
		// This is a large batch which was already added to the immutable queue.
		return nil
	}
	err := mem.apply(b, b.seqNum())
	if err != nil {
		return err
	}
	if mem.unref() {
		d.mu.Lock()
		d.maybeScheduleFlush()
		d.mu.Unlock()
	}
	return nil
}

func (d *DB) commitWrite(b *Batch, wg *sync.WaitGroup) (*memTable, error) {
	d.mu.Lock()

	if b.flushable != nil {
		b.flushable.seqNum = b.seqNum()
	}

	// Switch out the memtable if there was not enough room to store the batch.
	err := d.makeRoomForWrite(b)

	if err == nil {
		d.mu.log.bytesIn += uint64(len(b.storage.data))
	}

	d.mu.Unlock()
	if err != nil {
		return nil, err
	}

	if d.opts.DisableWAL {
		return d.mu.mem.mutable, nil
	}

	size, err := d.mu.log.SyncRecord(b.storage.data, wg)
	if err != nil {
		panic(err)
	}

	atomic.StoreUint64(&d.mu.log.size, uint64(size))
	return d.mu.mem.mutable, err
}

type iterAlloc struct {
	dbi             Iterator
	merging         mergingIter
	iters           [3 + numLevels]internalIterator
	rangeDelIters   [3 + numLevels]internalIterator
	largestUserKeys [3 + numLevels][]byte
	levels          [numLevels]levelIter
}

var iterAllocPool = sync.Pool{
	New: func() interface{} {
		return &iterAlloc{}
	},
}

// newIterInternal constructs a new iterator, merging in batchIter as an extra
// level.
func (d *DB) newIterInternal(
	batchIter internalIterator,
	batchRangeDelIter internalIterator,
	s *Snapshot,
	o *IterOptions,
) *Iterator {
	if atomic.LoadInt32(&d.closed) != 0 {
		panic(ErrClosed)
	}

	// Grab and reference the current readState. This prevents the underlying
	// files in the associated version from being deleted if there is a current
	// compaction. The readState is unref'd by Iterator.Close().
	readState := d.loadReadState()

	// Determine the seqnum to read at after grabbing the read state (current and
	// memtables) above.
	var seqNum uint64
	if s != nil {
		seqNum = s.seqNum
	} else {
		seqNum = atomic.LoadUint64(&d.mu.versions.visibleSeqNum)
	}

	// Bundle various structures under a single umbrella in order to allocate
	// them together.
	buf := iterAllocPool.Get().(*iterAlloc)
	dbi := &buf.dbi
	dbi.alloc = buf
	dbi.cmp = d.cmp
	dbi.equal = d.equal
	dbi.merge = d.merge
	dbi.split = d.split
	dbi.readState = readState
	if o != nil {
		dbi.opts = *o
	}

	iters := buf.iters[:0]
	rangeDelIters := buf.rangeDelIters[:0]
	largestUserKeys := buf.largestUserKeys[:0]
	if batchIter != nil {
		iters = append(iters, batchIter)
		rangeDelIters = append(rangeDelIters, batchRangeDelIter)
		largestUserKeys = append(largestUserKeys, nil)
	}

	// TODO(peter): We only need to add memtables which contain sequence numbers
	// older than seqNum. Unfortunately, memtables don't track their oldest
	// sequence number currently.
	memtables := readState.memtables
	for i := len(memtables) - 1; i >= 0; i-- {
		mem := memtables[i]
		iters = append(iters, mem.newIter(&dbi.opts))
		rangeDelIters = append(rangeDelIters, mem.newRangeDelIter(&dbi.opts))
		largestUserKeys = append(largestUserKeys, nil)
	}

	// The level 0 files need to be added from newest to oldest.
	current := readState.current
	for i := len(current.files[0]) - 1; i >= 0; i-- {
		f := &current.files[0][i]
		iter, rangeDelIter, err := d.newIters(f, &dbi.opts, nil)
		if err != nil {
			dbi.err = err
			return dbi
		}
		iters = append(iters, iter)
		rangeDelIters = append(rangeDelIters, rangeDelIter)
		largestUserKeys = append(largestUserKeys, nil)
	}

	start := len(rangeDelIters)
	for level := 1; level < len(current.files); level++ {
		if len(current.files[level]) == 0 {
			continue
		}
		rangeDelIters = append(rangeDelIters, nil)
		largestUserKeys = append(largestUserKeys, nil)
	}
	buf.merging.rangeDelIters = rangeDelIters
	buf.merging.largestUserKeys = largestUserKeys
	rangeDelIters = rangeDelIters[start:]
	largestUserKeys = largestUserKeys[start:]

	// Add level iterators for the remaining files.
	levels := buf.levels[:]
	for level := 1; level < len(current.files); level++ {
		if len(current.files[level]) == 0 {
			continue
		}

		var li *levelIter
		if len(levels) > 0 {
			li = &levels[0]
			levels = levels[1:]
		} else {
			li = &levelIter{}
		}

		li.init(&dbi.opts, d.cmp, d.newIters, current.files[level], nil)
		li.initRangeDel(&rangeDelIters[0])
		li.initLargestUserKey(&largestUserKeys[0])
		iters = append(iters, li)
		rangeDelIters = rangeDelIters[1:]
		largestUserKeys = largestUserKeys[1:]
	}

	buf.merging.init(d.cmp, iters...)
	buf.merging.snapshot = seqNum
	dbi.iter = &buf.merging
	return dbi
}

// NewBatch returns a new empty write-only batch. Any reads on the batch will
// return an error. If the batch is committed it will be applied to the DB.
func (d *DB) NewBatch() *Batch {
	return newBatch(d)
}

// NewIndexedBatch returns a new empty read-write batch. Any reads on the batch
// will read from both the batch and the DB. If the batch is committed it will
// be applied to the DB. An indexed batch is slower that a non-indexed batch
// for insert operations. If you do not need to perform reads on the batch, use
// NewBatch instead.
func (d *DB) NewIndexedBatch() *Batch {
	return newIndexedBatch(d, d.opts.Comparer)
}

// NewIter returns an iterator that is unpositioned (Iterator.Valid() will
// return false). The iterator can be positioned via a call to SeekGE, SeekLT,
// First or Last. The iterator provides a point-in-time view of the current DB
// state. This view is maintained by preventing file deletions and preventing
// memtables referenced by the iterator from being deleted. Using an iterator
// to maintain a long-lived point-in-time view of the DB state can lead to an
// apparent memory and disk usage leak. Use snapshots (see NewSnapshot) for
// point-in-time snapshots which avoids these problems.
func (d *DB) NewIter(o *IterOptions) *Iterator {
	return d.newIterInternal(nil, /* batchIter */
		nil /* batchRangeDelIter */, nil /* snapshot */, o)
}

// NewSnapshot returns a point-in-time view of the current DB state. Iterators
// created with this handle will all observe a stable snapshot of the current
// DB state. The caller must call Snapshot.Close() when the snapshot is no
// longer needed. Snapshots are not persisted across DB restarts (close ->
// open). Unlike the implicit snapshot maintained by an iterator, a snapshot
// will not prevent memtables from being released or sstables from being
// deleted. Instead, a snapshot prevents deletion of sequence numbers
// referenced by the snapshot.
func (d *DB) NewSnapshot() *Snapshot {
	if atomic.LoadInt32(&d.closed) != 0 {
		panic(ErrClosed)
	}

	s := &Snapshot{
		db:     d,
		seqNum: atomic.LoadUint64(&d.mu.versions.visibleSeqNum),
	}
	d.mu.Lock()
	d.mu.snapshots.pushBack(s)
	d.mu.Unlock()
	return s
}

// Close closes the DB.
//
// It is not safe to close a DB until all outstanding iterators are closed.
// It is valid to call Close multiple times. Other methods should not be
// called after the DB has been closed.
func (d *DB) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if atomic.LoadInt32(&d.closed) != 0 {
		panic(ErrClosed)
	}
	atomic.StoreInt32(&d.closed, 1)
	for d.mu.compact.compacting || d.mu.compact.flushing {
		d.mu.compact.cond.Wait()
	}
	err := d.tableCache.Close()
	err = firstError(err, d.mu.log.Close())
	err = firstError(err, d.fileLock.Close())
	d.commit.Close()

	err = firstError(err, d.dataDir.Close())

	if err == nil {
		d.readState.val.unrefLocked()

		current := d.mu.versions.currentVersion()
		for v := d.mu.versions.versions.front(); true; v = v.next {
			refs := atomic.LoadInt32(&v.refs)
			if v == current {
				if refs != 1 {
					return fmt.Errorf("leaked iterators: current\n%s", v)
				}
				break
			}
			if refs != 0 {
				return fmt.Errorf("leaked iterators:\n%s", v)
			}
		}
	}
	return err
}

// Compact the specified range of keys in the database.
func (d *DB) Compact(start, end []byte /* CompactionOptions */) error {
	if atomic.LoadInt32(&d.closed) != 0 {
		panic(ErrClosed)
	}

	iStart := base.MakeInternalKey(start, InternalKeySeqNumMax, InternalKeyKindMax)
	iEnd := base.MakeInternalKey(end, 0, 0)
	meta := []*fileMetadata{&fileMetadata{smallest: iStart, largest: iEnd}}

	d.mu.Lock()
	maxLevelWithFiles := 1
	cur := d.mu.versions.currentVersion()
	for level := 0; level < numLevels; level++ {
		if len(cur.overlaps(level, d.cmp, start, end)) > 0 {
			maxLevelWithFiles = level + 1
		}
	}

	// Determine if any memtable overlaps with the compaction range. We wait for
	// any such overlap to flush (initiating a flush if necessary).
	mem, err := func() (flushable, error) {
		if ingestMemtableOverlaps(d.cmp, d.mu.mem.mutable, meta) {
			mem := d.mu.mem.mutable
			return mem, d.makeRoomForWrite(nil)
		}
		// Check to see if any files overlap with any of the immutable
		// memtables. The queue is ordered from oldest to newest. We want to wait
		// for the newest table that overlaps.
		for i := len(d.mu.mem.queue) - 1; i >= 0; i-- {
			mem := d.mu.mem.queue[i]
			if ingestMemtableOverlaps(d.cmp, mem, meta) {
				return mem, nil
			}
		}
		return nil, nil
	}()

	d.mu.Unlock()

	if err != nil {
		return err
	}
	if mem != nil {
		<-mem.flushed()
	}

	for level := 0; level < maxLevelWithFiles; {
		manual := &manualCompaction{
			done:  make(chan error, 1),
			level: level,
			start: iStart,
			end:   iEnd,
		}
		if err := d.manualCompact(manual); err != nil {
			return err
		}
		level = manual.outputLevel
		if level == numLevels-1 {
			// A manual compaction of the bottommost level occured. There is no next
			// level to try and compact.
			break
		}
	}
	return nil
}

func (d *DB) manualCompact(manual *manualCompaction) error {
	d.mu.Lock()
	d.mu.compact.manual = append(d.mu.compact.manual, manual)
	d.maybeScheduleCompaction()
	d.mu.Unlock()
	return <-manual.done
}

// Flush the memtable to stable storage.
func (d *DB) Flush() error {
	if atomic.LoadInt32(&d.closed) != 0 {
		panic(ErrClosed)
	}

	d.mu.Lock()
	mem := d.mu.mem.mutable
	err := d.makeRoomForWrite(nil)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	<-mem.flushed()
	return nil
}

// AsyncFlush asynchronously flushes the memtable to stable storage.
//
// TODO(peter): untested
func (d *DB) AsyncFlush() error {
	if atomic.LoadInt32(&d.closed) != 0 {
		panic(ErrClosed)
	}

	d.mu.Lock()
	err := d.makeRoomForWrite(nil)
	d.mu.Unlock()
	return err
}

// Metrics returns metrics about the database.
func (d *DB) Metrics() *VersionMetrics {
	metrics := &VersionMetrics{}
	recycledLogs := d.logRecycler.count()
	d.mu.Lock()
	*metrics = d.mu.versions.metrics
	metrics.WAL.ObsoleteFiles = int64(recycledLogs)
	metrics.WAL.Size = atomic.LoadUint64(&d.mu.log.size)
	metrics.WAL.BytesIn = d.mu.log.bytesIn // protected by d.mu
	for i, n := 0, len(d.mu.mem.queue)-1; i < n; i++ {
		_, size := d.mu.mem.queue[i].logInfo()
		metrics.WAL.Size += size
	}
	metrics.WAL.BytesWritten = metrics.Levels[0].BytesIn + metrics.WAL.Size
	metrics.Levels[0].Score = float64(metrics.Levels[0].NumFiles) / float64(d.opts.L0CompactionThreshold)
	if p := d.mu.versions.picker; p != nil {
		for level := 1; level < numLevels; level++ {
			metrics.Levels[level].Score = float64(metrics.Levels[level].Size) / float64(p.levelMaxBytes[level])
		}
	}
	d.mu.Unlock()
	return metrics
}

func (d *DB) walPreallocateSize() int {
	// Set the WAL preallocate size to 110% of the memtable size. Note that there
	// is a bit of apples and oranges in units here as the memtabls size
	// corresponds to the memory usage of the memtable while the WAL size is the
	// size of the batches (plus overhead) stored in the WAL.
	//
	// TODO(peter): 110% of the memtable size is quite hefty for a block
	// size. This logic is taken from GetWalPreallocateBlockSize in
	// RocksDB. Could a smaller preallocation block size be used?
	size := d.opts.MemTableSize
	size = (size / 10) + size
	return size
}

func (d *DB) makeRoomForWrite(b *Batch) error {
	force := b == nil || b.flushable != nil
	for {
		if d.mu.mem.switching {
			d.mu.mem.cond.Wait()
			continue
		}
		if b != nil && b.flushable == nil {
			err := d.mu.mem.mutable.prepare(b)
			if err == nil {
				return nil
			}
			if err != arenaskl.ErrArenaFull {
				return err
			}
		} else if !force {
			return nil
		}
		if len(d.mu.mem.queue) >= d.opts.MemTableStopWritesThreshold {
			// We have filled up the current memtable, but the previous one is still
			// being compacted, so we wait.
			// fmt.Printf("memtable stop writes threshold\n")
			d.mu.compact.cond.Wait()
			continue
		}
		if len(d.mu.versions.currentVersion().files[0]) > d.opts.L0StopWritesThreshold {
			// There are too many level-0 files, so we wait.
			// fmt.Printf("L0 stop writes threshold\n")
			d.mu.compact.cond.Wait()
			continue
		}

		var newLogNumber uint64
		var newLogFile vfs.File
		var prevLogSize uint64
		var err error

		if !d.opts.DisableWAL {
			jobID := d.mu.nextJobID
			d.mu.nextJobID++
			newLogNumber = d.mu.versions.nextFileNum()
			d.mu.mem.switching = true
			d.mu.Unlock()

			newLogName := dbFilename(d.walDirname, fileTypeLog, newLogNumber)

			// Try to use a recycled log file. Recycling log files is an important
			// performance optimization as it is faster to sync a file that has
			// already been written, than one which is being written for the first
			// time. This is due to the need to sync file metadata when a file is
			// being written for the first time. Note this is true even if file
			// preallocation is performed (e.g. fallocate).
			recycleLogNumber := d.logRecycler.peek()
			if recycleLogNumber > 0 {
				recycleLogName := dbFilename(d.walDirname, fileTypeLog, recycleLogNumber)
				err = d.opts.FS.Rename(recycleLogName, newLogName)
			}

			if err == nil {
				newLogFile, err = d.opts.FS.Create(newLogName)
			}

			if err == nil {
				// TODO(peter): RocksDB delays sync of the parent directory until the
				// first time the log is synced. Is that worthwhile?
				err = d.walDir.Sync()
			}

			if err == nil {
				prevLogSize = uint64(d.mu.log.Size())
				err = d.mu.log.Close()
				if err != nil {
					newLogFile.Close()
				} else {
					newLogFile = vfs.NewSyncingFile(newLogFile, vfs.SyncingFileOptions{
						BytesPerSync:    d.opts.BytesPerSync,
						PreallocateSize: d.walPreallocateSize(),
					})
				}
			}

			if recycleLogNumber > 0 {
				err = d.logRecycler.pop(recycleLogNumber)
			}

			if d.opts.EventListener.WALCreated != nil {
				d.opts.EventListener.WALCreated(WALCreateInfo{
					JobID:           jobID,
					Path:            newLogName,
					FileNum:         newLogNumber,
					RecycledFileNum: recycleLogNumber,
					Err:             err,
				})
			}

			d.mu.Lock()
			d.mu.mem.switching = false
			d.mu.mem.cond.Broadcast()

			d.mu.versions.metrics.WAL.Files++
		}

		if err != nil {
			// TODO(peter): avoid chewing through file numbers in a tight loop if there
			// is an error here.
			//
			// What to do here? Stumbling on doesn't seem worthwhile. If we failed to
			// close the previous log it is possible we lost a write.
			panic(err)
		}

		if !d.opts.DisableWAL {
			d.mu.log.queue = append(d.mu.log.queue, newLogNumber)
			d.mu.log.LogWriter = record.NewLogWriter(newLogFile, newLogNumber)
		}

		imm := d.mu.mem.mutable
		imm.logSize = prevLogSize
		prevLogNumber := imm.logNum

		var scheduleFlush bool
		if b != nil && b.flushable != nil {
			// The batch is too large to fit in the memtable so add it directly to
			// the immutable queue.
			b.flushable.logNum = prevLogNumber
			d.mu.mem.queue = append(d.mu.mem.queue, b.flushable)
			scheduleFlush = true
		}

		// Create a new memtable, scheduling the previous one for flushing. We do
		// this even if the previous memtable was empty because the DB.Flush
		// mechanism is dependent on being able to wait for the empty memtable to
		// flush. We can't just mark the empty memtable as flushed here because we
		// also have to wait for all previous immutable tables to
		// flush. Additionally, the memtable is tied to particular WAL file and we
		// want to go through the flush path in order to recycle that WAL file.
		d.mu.mem.mutable = newMemTable(d.opts)
		// NB: When the immutable memtable is flushed to disk it will apply a
		// versionEdit to the manifest telling it that log files < newLogNumber
		// have been applied. newLogNumber corresponds to the WAL that contains
		// mutations that are present in the new memtable.
		d.mu.mem.mutable.logNum = newLogNumber
		d.mu.mem.queue = append(d.mu.mem.queue, d.mu.mem.mutable)
		d.updateReadStateLocked()
		if (imm != nil && imm.unref()) || scheduleFlush {
			d.maybeScheduleFlush()
		}
		force = false
	}
}

// firstError returns the first non-nil error of err0 and err1, or nil if both
// are nil.
func firstError(err0, err1 error) error {
	if err0 != nil {
		return err0
	}
	return err1
}
