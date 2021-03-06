// Package gaio is an Async-IO library for Golang.
//
// gaio acts in proactor mode, https://en.wikipedia.org/wiki/Proactor_pattern.
// User submit async IO operations and waits for IO-completion signal.
package gaio

import (
	"container/heap"
	"container/list"
	"errors"
	"net"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrUnsupported means the watcher cannot support this type of connection
	ErrUnsupported = errors.New("unsupported connection, must be pointer")
	// ErrNoRawConn means the connection has not implemented SyscallConn
	ErrNoRawConn = errors.New("net.Conn does implement net.RawConn")
	// ErrWatcherClosed means the watcher is closed
	ErrWatcherClosed = errors.New("watcher closed")
	// ErrConnClosed means the user called Free() on related connection
	ErrConnClosed = errors.New("connection closed")
	// ErrDeadline means the specific operation has exceeded deadline before completion
	ErrDeadline = errors.New("operation exceeded deadline")
	// ErrEmptyBuffer means the buffer is nil
	ErrEmptyBuffer = errors.New("empty buffer")
)

var (
	zeroTime = time.Time{}
)

// OpType defines Operation Type
type OpType int

const (
	// OpRead means the aiocb is a read operation
	OpRead OpType = iota
	// OpWrite means the aiocb is a write operation
	OpWrite
	// internal operation to delete an related resource
	opDelete
)

// aiocb contains all info for a request
type aiocb struct {
	l        *list.List // list where this request belongs to
	elem     *list.Element
	ctx      interface{} // user context associated with this request
	ptr      uintptr     // pointer to conn
	op       OpType      // read or write
	conn     net.Conn    // associated connection for nonblocking-io
	err      error       // error for last operation
	size     int         // size received or sent
	buffer   []byte
	useSwap  bool // mark if the buffer is internal swap
	idx      int  // index for heap op
	deadline time.Time
}

// readable & writable bitmask
const (
	fdRead  byte = 1
	fdWrite      = 2
)

// fdDesc contains all info related to fd
type fdDesc struct {
	status  byte      // fd read/write status
	readers list.List // all read/write requests
	writers list.List
	ptr     uintptr // pointer to net.Conn
}

// OpResult is the result of an aysnc-io
type OpResult struct {
	// Operation Type
	Operation OpType
	// User context associated with this requests
	Context interface{}
	// Related net.Conn to this result
	Conn net.Conn
	// Buffer points to user's supplied buffer or watcher's internal swap buffer
	Buffer []byte
	// Number of bytes sent or received, Buffer[:Size] is the content sent or received.
	Size int
	// IO error,timeout error
	Error error
}

// Watcher will monitor events and process async-io request(s),
type Watcher struct {
	// poll fd
	pfd *poller

	// netpoll events
	chEventNotify chan pollerEvents

	// events from user
	chPendingNotify chan struct{}

	// IO-completion events to user
	chNotifyCompletion chan []OpResult
	swapResults        [][]OpResult
	swapIdx            int

	// lock for pending io operations
	// aiocb is associated to fd
	pending      []*aiocb
	pendingMutex sync.Mutex

	// internal buffer for reading
	swapBuffer     [][]byte
	nextSwapBuffer int

	die     chan struct{}
	dieOnce sync.Once
}

// NewWatcher creates a management object for monitoring file descriptors
// 'bufsize' sets the internal swap buffer size for Read() with nil.
func NewWatcherSize(bufsize int) (*Watcher, error) {
	w := new(Watcher)
	pfd, err := openPoll()
	if err != nil {
		return nil, err
	}
	w.pfd = pfd

	// loop related chan
	w.chEventNotify = make(chan pollerEvents)
	w.chPendingNotify = make(chan struct{}, 1)
	w.chNotifyCompletion = make(chan []OpResult)
	w.die = make(chan struct{})

	// swapBuffer for shared reading
	w.swapBuffer = make([][]byte, 2)
	for i := 0; i < len(w.swapBuffer); i++ {
		w.swapBuffer[i] = make([]byte, bufsize)
	}

	// swapResults for batch notification
	w.swapResults = make([][]OpResult, 2)
	for i := 0; i < len(w.swapResults); i++ {
		w.swapResults[i] = make([]OpResult, 0, maxEvents)
	}

	// finalizer for system resources
	runtime.SetFinalizer(w, func(w *Watcher) {
		close(w.die)
		w.pfd.Close()
	})

	go w.pfd.Wait(w.chEventNotify, w.die)
	go w.loop()
	return w, nil
}

// Close stops monitoring on events for all connections
func (w *Watcher) Close() (err error) {
	runtime.SetFinalizer(w, nil)
	w.dieOnce.Do(func() {
		close(w.die)
		err = w.pfd.Close()
	})
	return err
}

// notify new operations pending
func (w *Watcher) notifyPending() {
	select {
	case w.chPendingNotify <- struct{}{}:
	default:
	}
}

// WaitIO blocks until any read/write completion, or error
func (w *Watcher) WaitIO() (r []OpResult, err error) {
	select {
	case r := <-w.chNotifyCompletion:
		return r, nil
	case <-w.die:
		return r, ErrWatcherClosed
	}
}

// Read submits an async read request on 'fd' with context 'ctx', using buffer 'buf'.
// 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *Watcher) Read(ctx interface{}, conn net.Conn, buf []byte) error {
	return w.aioCreate(ctx, OpRead, conn, buf, zeroTime)
}

// ReadTimeout submits an async read request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *Watcher) ReadTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	return w.aioCreate(ctx, OpRead, conn, buf, deadline)
}

// Write submits an async write request on 'fd' with context 'ctx', using buffer 'buf'.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *Watcher) Write(ctx interface{}, conn net.Conn, buf []byte) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, zeroTime)
}

// WriteTimeout submits an async write request on 'fd' with context 'ctx', using buffer 'buf', and
// expected to be completed before 'deadline', 'buf' can be set to nil to use internal buffer.
// 'ctx' is the user-defined value passed through the gaio watcher unchanged.
func (w *Watcher) WriteTimeout(ctx interface{}, conn net.Conn, buf []byte, deadline time.Time) error {
	if len(buf) == 0 {
		return ErrEmptyBuffer
	}
	return w.aioCreate(ctx, OpWrite, conn, buf, deadline)
}

// Free let the watcher to release resources related to this conn immediately,
// like socket file descriptors.
func (w *Watcher) Free(conn net.Conn) error {
	return w.aioCreate(nil, opDelete, conn, nil, zeroTime)
}

// core async-io creation
func (w *Watcher) aioCreate(ctx interface{}, op OpType, conn net.Conn, buf []byte, deadline time.Time) error {
	select {
	case <-w.die:
		return ErrWatcherClosed
	default:
		var ptr uintptr
		if reflect.TypeOf(conn).Kind() == reflect.Ptr {
			ptr = reflect.ValueOf(conn).Pointer()
		} else {
			return ErrUnsupported
		}
		w.pendingMutex.Lock()
		w.pending = append(w.pending, &aiocb{op: op, ptr: ptr, ctx: ctx, conn: conn, buffer: buf, deadline: deadline})
		w.pendingMutex.Unlock()

		w.notifyPending()
		return nil
	}
}

// tryRead will try to read data on aiocb and notify
func (w *Watcher) tryRead(fd int, pcb *aiocb) bool {
	buf := pcb.buffer

	var useSwap bool
	if buf == nil { // internal buffer
		buf = w.swapBuffer[w.nextSwapBuffer]
		useSwap = true
	}

	for {
		// return values are stored in pcb
		pcb.size, pcb.err = syscall.Read(fd, buf)
		if pcb.err == syscall.EAGAIN {
			return false
		}

		// On MacOS we can see EINTR here if the user
		// pressed ^Z.
		if pcb.err == syscall.EINTR {
			continue
		}
		break
	}

	// IO completed
	if useSwap {
		pcb.buffer = buf
		pcb.useSwap = true
		w.nextSwapBuffer = (w.nextSwapBuffer + 1) % len(w.swapBuffer)
	}

	return true
}

func (w *Watcher) tryWrite(fd int, pcb *aiocb) bool {
	var nw int
	var ew error

	if pcb.buffer != nil {
		nw, ew = syscall.Write(fd, pcb.buffer[pcb.size:])
		pcb.err = ew
		if ew == syscall.EAGAIN {
			return false
		}

		// if ew is nil, accumulate bytes written
		if ew == nil {
			pcb.size += nw
		}
	}

	// all bytes written or has error
	// nil buffer still returns
	if pcb.size == len(pcb.buffer) || ew != nil {
		return true
	}
	return false
}

// the core event loop of this watcher
func (w *Watcher) loop() {
	// all descriptors
	descs := make(map[int]*fdDesc)
	// we must not hold net.Conn as key, for GC purpose
	connIdents := make(map[uintptr]int)
	gc := make(chan uintptr)

	// for timeout operations
	// aiocb has non-zero deadline exists in timeouts & queue
	// at same time or in neither of them
	timer := time.NewTimer(0)
	var timeouts timedHeap

	releaseConn := func(ident int) {
		if desc, ok := descs[ident]; ok {
			// delete from heap
			for e := desc.readers.Front(); e != nil; e = e.Next() {
				tcb := e.Value.(*aiocb)
				if !tcb.deadline.IsZero() {
					heap.Remove(&timeouts, tcb.idx)
				}
			}

			for e := desc.writers.Front(); e != nil; e = e.Next() {
				tcb := e.Value.(*aiocb)
				if !tcb.deadline.IsZero() {
					heap.Remove(&timeouts, tcb.idx)
				}
			}

			delete(descs, ident)
			delete(connIdents, desc.ptr)
			// close socket file descriptor duplicated from net.Conn
			syscall.Close(ident)
		}
	}

	// release all resources
	defer func() {
		for ident := range descs {
			releaseConn(ident)
		}
	}()

	var pending []*aiocb
	for {
		select {
		case <-w.chPendingNotify:
			// copy from w.pending to local pending
			w.pendingMutex.Lock()
			if cap(pending) < cap(w.pending) {
				pending = make([]*aiocb, 0, cap(w.pending))
			}
			pending = pending[:len(w.pending)]
			copy(pending, w.pending)
			w.pending = w.pending[:0]
			w.pendingMutex.Unlock()

			for _, pcb := range pending {
				ident, ok := connIdents[pcb.ptr]
				// resource release
				if pcb.op == opDelete && ok {
					releaseConn(ident)
					continue
				}

				// new conn
				var desc *fdDesc
				if ok {
					desc = descs[ident]
				} else {
					if dupfd, err := dupconn(pcb.conn); err != nil {
						select {
						case w.chNotifyCompletion <- []OpResult{{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: 0, Error: err, Context: pcb.ctx}}:
						case <-w.die:
							return
						}
						continue
					} else {
						// assign idents
						ident = dupfd

						// unexpected situation, should notify caller
						werr := w.pfd.Watch(ident)
						if werr != nil {
							select {
							case w.chNotifyCompletion <- []OpResult{{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: 0, Error: werr, Context: pcb.ctx}}:
							case <-w.die:
								return
							}
							continue
						}

						// bindings
						desc = &fdDesc{ptr: pcb.ptr}
						descs[ident] = desc
						connIdents[pcb.ptr] = ident
						// as we duplicated succesfuly, we're safe to
						// close the original connection
						pcb.conn.Close()

						// the conn is still useful for GC finalizer
						// note finalizer function cannot hold reference to net.Conn
						// if not it will never be GC-ed
						runtime.SetFinalizer(pcb.conn, func(c net.Conn) {
							select {
							case gc <- reflect.ValueOf(c).Pointer():
							case <-w.die:
							}
						})
					}
				}

				// operations splitted into different buckets
				switch pcb.op {
				case OpRead:
					if desc.readers.Len() == 0 && desc.status&fdRead > 0 {
						if w.tryRead(ident, pcb) {
							select {
							case w.chNotifyCompletion <- []OpResult{{Operation: OpRead, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx}}:
							case <-w.die:
								return
							}
							if pcb.err != nil || (pcb.size == 0 && pcb.err == nil) {
								releaseConn(ident)
							}
							continue
						} else {
							desc.status &^= fdRead
						}
					}
					pcb.l = &desc.readers
					pcb.elem = pcb.l.PushBack(pcb)
				case OpWrite:
					if desc.writers.Len() == 0 && desc.status&fdWrite > 0 {
						if w.tryWrite(ident, pcb) {
							select {
							case w.chNotifyCompletion <- []OpResult{{Operation: OpWrite, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx}}:
							case <-w.die:
								return
							}
							if pcb.err != nil {
								releaseConn(ident)
							}
							continue
						} else {
							desc.status &^= fdWrite
						}
					}
					pcb.l = &desc.writers
					pcb.elem = pcb.l.PushBack(pcb)
				}

				// timer
				if !pcb.deadline.IsZero() {
					heap.Push(&timeouts, pcb)
					if timeouts.Len() == 1 {
						timer.Reset(pcb.deadline.Sub(time.Now()))
					}
				}
			}
			pending = pending[:0]
		case pe := <-w.chEventNotify:
			// suppose fd(s) being polled is closed by conn.Close() from outside after chanrecv,
			// and a new conn has re-opened with the same handler number(fd). The read and write
			// on this fd is fatal.
			//
			// Note poller will remove closed fd automatically epoll(7), kqueue(2) and silently.
			// To solve this problem watcher will dup() a new fd from net.Conn, which uniquely
			// identified by 'e.ident', all library operation will be based on 'e.ident',
			// then IO operation is impossible to misread or miswrite on re-created fd.
			//log.Println(e)
			results := w.swapResults[w.swapIdx][:0]
			for _, e := range pe {
				if desc, ok := descs[e.ident]; ok {
					var shouldRelease bool
					if e.r {
						desc.status |= fdRead
						var next *list.Element
						for elem := desc.readers.Front(); elem != nil; elem = next {
							next = elem.Next()
							pcb := elem.Value.(*aiocb)
							if w.tryRead(e.ident, pcb) {
								results = append(results, OpResult{Operation: OpRead, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
								// for shared memory, we need to notify WaitIO immediately
								if pcb.useSwap {
									select {
									case w.chNotifyCompletion <- results:
										w.swapIdx = (w.swapIdx + 1) % len(w.swapResults)
										results = w.swapResults[w.swapIdx][:0]
									case <-w.die:
										return
									}
								}
								desc.readers.Remove(elem)
								if !pcb.deadline.IsZero() {
									heap.Remove(&timeouts, pcb.idx)
								}
								if pcb.err != nil || (pcb.size == 0 && pcb.err == nil) {
									shouldRelease = true
									break
								}
							} else {
								desc.status &^= fdRead
								break
							}
						}
					}

					if e.w {
						desc.status |= fdWrite
						var next *list.Element
						for elem := desc.writers.Front(); elem != nil; elem = next {
							next = elem.Next()
							pcb := elem.Value.(*aiocb)
							if w.tryWrite(e.ident, pcb) {
								results = append(results, OpResult{Operation: OpWrite, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: pcb.err, Context: pcb.ctx})
								desc.writers.Remove(elem)
								if !pcb.deadline.IsZero() {
									heap.Remove(&timeouts, pcb.idx)
								}
								if pcb.err != nil {
									shouldRelease = true
									break
								}
							} else {
								desc.status &^= fdWrite
								break
							}
						}
					}

					if shouldRelease {
						releaseConn(e.ident)
					}
				}
			}

			// batch notification
			if len(results) > 0 {
				select {
				case w.chNotifyCompletion <- results:
					w.swapIdx = (w.swapIdx + 1) % len(w.swapResults)
				case <-w.die:
					return
				}
			}

		case <-timer.C:
			for timeouts.Len() > 0 {
				now := time.Now()
				pcb := timeouts[0]
				if now.After(pcb.deadline) {
					// remove from list
					pcb.l.Remove(pcb.elem)
					// ErrDeadline
					select {
					case w.chNotifyCompletion <- []OpResult{{Operation: pcb.op, Conn: pcb.conn, Buffer: pcb.buffer, Size: pcb.size, Error: ErrDeadline, Context: pcb.ctx}}:
					case <-w.die:
						return
					}
					heap.Pop(&timeouts)
				} else {
					timer.Reset(pcb.deadline.Sub(now))
					break
				}
			}
		case ptr := <-gc: // gc recycled net.Conn
			if ident, ok := connIdents[ptr]; ok {
				// since it's gc-ed, queue is impossible to hold net.Conn
				// we don't have to send to chIOCompletion,just release here
				releaseConn(ident)
			}
		case <-w.die:
			return
		}
	}
}
