package codexonly

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync"
)

type storageResponseWriter struct {
	http.ResponseWriter
	failures  *storageFailureState
	mu        sync.Mutex
	committed bool
	blocked   bool
	fatalErr  error
}

func newStorageResponseWriter(w http.ResponseWriter, failures *storageFailureState) *storageResponseWriter {
	return &storageResponseWriter{
		ResponseWriter: w,
		failures:       failures,
	}
}

func (w *storageResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeHeaderLocked(status)
}

func (w *storageResponseWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.committed {
		w.writeHeaderLocked(http.StatusOK)
	}
	if w.blocked {
		return len(data), nil
	}
	return w.ResponseWriter.Write(data)
}

func (w *storageResponseWriter) Flush() {
	_ = w.FlushError()
}

func (w *storageResponseWriter) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.committed {
		w.writeHeaderLocked(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(interface{ FlushError() error }); ok {
		return flusher.FlushError()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
		return nil
	}
	return http.ErrNotSupported
}

func (w *storageResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.blocked {
		return nil, nil, w.fatalErr
	}
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	if w.committed {
		return hijacker.Hijack()
	}

	var conn net.Conn
	var readWriter *bufio.ReadWriter
	var errHijack error
	if fatalErr := w.failures.commit(func() {
		conn, readWriter, errHijack = hijacker.Hijack()
	}); fatalErr != nil {
		w.writeFatalLocked(fatalErr)
		return nil, nil, fatalErr
	}
	if errHijack == nil {
		w.committed = true
	}
	return conn, readWriter, errHijack
}

func (w *storageResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	var errPush error
	if fatalErr := w.failures.commit(func() {
		errPush = pusher.Push(target, opts)
	}); fatalErr != nil {
		return fatalErr
	}
	return errPush
}

func (w *storageResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	w.mu.Lock()
	if !w.committed {
		w.writeHeaderLocked(http.StatusOK)
	}
	if w.blocked {
		err := w.fatalErr
		w.mu.Unlock()
		return 0, err
	}
	target := w.ResponseWriter
	w.mu.Unlock()

	if readerFrom, ok := target.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(src)
	}
	return io.Copy(target, src)
}

func (w *storageResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *storageResponseWriter) writeHeaderLocked(status int) {
	if w.committed {
		return
	}
	if status < http.StatusBadRequest {
		if fatalErr := w.failures.commit(func() {
			w.ResponseWriter.WriteHeader(status)
		}); fatalErr != nil {
			w.writeFatalLocked(fatalErr)
			return
		}
	} else {
		w.ResponseWriter.WriteHeader(status)
	}
	if status >= http.StatusOK || status == http.StatusSwitchingProtocols {
		w.committed = true
	}
}

func (w *storageResponseWriter) writeFatalLocked(err error) {
	header := w.ResponseWriter.Header()
	for key := range header {
		header.Del(key)
	}
	writeError(w.ResponseWriter, http.StatusInternalServerError, "internal server error")
	w.committed = true
	w.blocked = true
	w.fatalErr = err
}
