package common

import (
	"bytes"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// autoAttemptResponseBufferLimit bounds per-attempt staging. It is a hard cap:
// once a chunk no longer fits, the already staged prefix is committed and every
// later chunk is streamed through directly. Staging never holds more than this.
const autoAttemptResponseBufferLimit = 256 * 1024

// autoAttemptKeepAliveData is the SSE comment written by PingData. Keepalive
// bytes are not answer content and must not enter the staging buffer: they
// would count against its limit and could be replayed verbatim when another
// candidate's response is committed.
var autoAttemptKeepAliveData = []byte(": PING\n\n")

var errAutoAttemptResponseDiscarded = errors.New("auto attempt response was discarded")

// AutoAttemptResponseWriter stages one auto candidate's response until the
// existing answer observer confirms actual output. It sits above the regular
// relayResponseWriter, which continues to own model/error rewriting.
//
// A buffered response can be discarded only before Commit. Once committed, the
// attempt is treated exactly like today's streaming path: no second candidate
// may be spliced into the client response.
//
// Writes may arrive from the relay goroutine and from SSE keepalive goroutines,
// so every mutable field is guarded by mu. A keepalive that arrives before
// commit is dropped instead of being staged: flushing it would commit a status
// line and make the candidate non-retryable.
type AutoAttemptResponseWriter struct {
	gin.ResponseWriter
	context       *gin.Context
	info          *RelayInfo
	relay         *relayResponseWriter
	initialHeader http.Header

	mu          sync.Mutex
	body        bytes.Buffer
	discarded   bool
	overflow    bool
	exploration *AutoExplorationAttempt
	// status/committed stay atomic because the underlying relay writer reads
	// c.Writer.Status()/Written() while a commit is in progress; taking mu there
	// would re-enter the write path and deadlock.
	status    atomic.Int32
	committed atomic.Bool
}

// InstallAutoAttemptResponseWriter installs a fresh staging writer for the next
// real upstream attempt. It refuses when the response has already started or
// when the relay writer is not the expected concrete wrapper.
func InstallAutoAttemptResponseWriter(c *gin.Context) *AutoAttemptResponseWriter {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return nil
	}
	relay, ok := c.Writer.(*relayResponseWriter)
	if !ok {
		return nil
	}
	w := &AutoAttemptResponseWriter{
		ResponseWriter: c.Writer,
		context:        c,
		info:           GetRelayInfo(c),
		relay:          relay,
		initialHeader:  cloneAutoAttemptHeader(c.Writer.Header()),
	}
	c.Writer = w
	return w
}

func cloneAutoAttemptHeader(header http.Header) http.Header {
	cloned := make(http.Header, len(header))
	for key, values := range header {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func (w *AutoAttemptResponseWriter) restoreHeaderLocked() {
	if w.context == nil || w.context.Writer == nil {
		return
	}
	header := w.context.Writer.Header()
	for key := range header {
		delete(header, key)
	}
	for key, values := range w.initialHeader {
		header[key] = append([]string(nil), values...)
	}
}

func (w *AutoAttemptResponseWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed.Load() {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.status.Load() == 0 {
		w.status.Store(int32(code))
	}
}

func (w *AutoAttemptResponseWriter) WriteHeaderNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed.Load() {
		w.ResponseWriter.WriteHeaderNow()
		return
	}
	if w.status.Load() == 0 {
		w.status.Store(http.StatusOK)
	}
}

func (w *AutoAttemptResponseWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.discarded {
		return 0, errAutoAttemptResponseDiscarded
	}
	if w.committed.Load() {
		if w.info != nil {
			w.info.ObserveClientAnswer(data)
		}
		return w.writeToClient(data)
	}
	if bytes.Equal(data, autoAttemptKeepAliveData) {
		// The candidate is not validated yet; a ping must neither be buffered
		// nor force the response to commit.
		return len(data), nil
	}
	if w.info != nil {
		w.info.ObserveClientAnswer(data)
	}
	if w.body.Len()+len(data) > autoAttemptResponseBufferLimit {
		// The size cap is a safety valve, not a switch point: commit the staged
		// prefix and stream this chunk through without buffering it.
		w.overflow = true
		if err := w.commitLocked(); err != nil {
			return 0, err
		}
		return w.writeToClient(data)
	}
	_, _ = w.body.Write(data)
	if w.info != nil && w.info.ClientAnswerHasValidOutput() {
		if err := w.commitLocked(); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func (w *AutoAttemptResponseWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func (w *AutoAttemptResponseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed.Load() {
		w.ResponseWriter.Flush()
	}
	// Before validation, a flush must not commit the candidate.
}

func (w *AutoAttemptResponseWriter) Status() int {
	if status := w.status.Load(); status != 0 {
		return int(status)
	}
	return http.StatusOK
}

func (w *AutoAttemptResponseWriter) Size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.committed.Load() {
		return w.body.Len()
	}
	return w.ResponseWriter.Size()
}

func (w *AutoAttemptResponseWriter) Written() bool {
	return w.committed.Load()
}

// SetExplorationAttempt captures this attempt's guard before dispatch. Writer
// goroutines must not read the RelayInfo pointer while the controller resets it
// for a subsequent attempt.
func (w *AutoAttemptResponseWriter) SetExplorationAttempt(attempt *AutoExplorationAttempt) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.exploration = attempt
}

// Commit delivers the staged status, headers and body exactly once.
func (w *AutoAttemptResponseWriter) Commit() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.commitLocked()
}

func (w *AutoAttemptResponseWriter) commitLocked() error {
	if w.committed.Load() {
		return nil
	}
	if w.discarded {
		return errAutoAttemptResponseDiscarded
	}
	if !w.exploration.protect() {
		return ErrAutoExplorationTimeout
	}
	status := int(w.status.Load())
	if status == 0 {
		status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(status)
	if w.body.Len() > 0 {
		if _, err := w.writeToClient(w.body.Bytes()); err != nil {
			return err
		}
	}
	w.body.Reset()
	w.committed.Store(true)
	return nil
}

// Discard drops an uncommitted candidate response and restores headers to the
// state they had before the attempt. It is a no-op after Commit.
func (w *AutoAttemptResponseWriter) Discard() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed.Load() || w.discarded {
		return
	}
	w.discarded = true
	w.body.Reset()
	w.restoreHeaderLocked()
}

// Restore puts the regular relay writer back on the gin context. It is safe to
// call after Commit or Discard and is idempotent.
func (w *AutoAttemptResponseWriter) Restore() {
	if w.context == nil {
		return
	}
	if w.context.Writer == w {
		w.context.Writer = w.ResponseWriter
	}
}

func (w *AutoAttemptResponseWriter) Committed() bool {
	return w.committed.Load()
}

func (w *AutoAttemptResponseWriter) Overflow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overflow
}

func (w *AutoAttemptResponseWriter) StagedBytes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Len()
}

func (w *AutoAttemptResponseWriter) writeToClient(data []byte) (int, error) {
	if w.relay != nil {
		return w.relay.writePreObserved(data)
	}
	n, err := w.ResponseWriter.Write(data)
	if err == nil && n == len(data) {
		return len(data), nil
	}
	markClientDeliveryBroken(w.context)
	return n, err
}

// writePreObserved rewrites and writes bytes that the staging layer already
// observed, avoiding a second ObserveClientAnswer pass (which would duplicate
// text fragments and could corrupt banner detection).
func (w *relayResponseWriter) writePreObserved(data []byte) (int, error) {
	rewritten := rewriteClientResponseBytes(w.context, data, &w.sseEventError)
	n, err := w.ResponseWriter.Write(rewritten)
	if err == nil && n == len(rewritten) {
		return len(data), nil
	}
	markClientDeliveryBroken(w.context)
	return n, err
}
