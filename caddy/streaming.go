// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Most of the code in this file was initially borrowed from the Go
// standard library and modified; It had this copyright notice:
// Copyright 2011 The Go Authors

// This code was largely taken from Caddy (https://github.com/caddyserver/caddy)
// at git hash e385be922569c07a0471a6798d4aeaf972facb5b.
// The above copyright notice is what it originally had.

package caddy

import (
	"io"
	"net/http"
	"sync"
	"time"
)

func WrapFlushResponseWriter(rw http.ResponseWriter, flushInterval time.Duration) http.ResponseWriter {
	mlw := maxLatencyWriter{
		dst:     rw.(writeFlusher),
		latency: flushInterval,
	}

	return &maxLatencyResponseWriter{rw, mlw}
}

// copyBuffer returns any write errors or non-EOF read errors, and the amount
// of bytes written.
func copyBuffer(dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	if len(buf) == 0 {
		buf = make([]byte, 32*1024)
	}
	var written int64
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if werr != nil {
				return written, werr
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				rerr = nil
			}
			return written, rerr
		}
	}
}

type writeFlusher interface {
	io.Writer
	http.Flusher
}

type maxLatencyWriter struct {
	dst     writeFlusher
	latency time.Duration // non-zero; negative means to flush immediately

	mu           sync.Mutex // protects t, flushPending, and dst.Flush
	t            *time.Timer
	flushPending bool
}

// Implement io.ReaderFrom, this will allow goproxy to io.Copy to it
// with the flushing interval applied.
var _ io.ReaderFrom = (*maxLatencyWriter)(nil)

func (m *maxLatencyWriter) ReadFrom(src io.Reader) (int64, error) {
	defer m.stop()

	buf := streamingBufPool.Get().([]byte)
	defer streamingBufPool.Put(buf)

	// Shouldn't use io.CopyBuffer as we would have an infinite loop.
	return copyBuffer(m, src, buf)
}

func (m *maxLatencyWriter) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err = m.dst.Write(p)
	if m.latency < 0 {
		m.dst.Flush()
		return
	}
	if m.flushPending {
		return
	}
	if m.t == nil {
		m.t = time.AfterFunc(m.latency, m.delayedFlush)
	} else {
		m.t.Reset(m.latency)
	}
	m.flushPending = true
	return
}

func (m *maxLatencyWriter) delayedFlush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.flushPending { // if stop was called but AfterFunc already started this goroutine
		return
	}
	m.dst.Flush()
	m.flushPending = false
}

func (m *maxLatencyWriter) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushPending = false
	if m.t != nil {
		m.t.Stop()
	}
}

// Passes normal response writer calls to the original.
// But handles writes / copies using maxLatencyWriter.
type maxLatencyResponseWriter struct {
	// Important to NOT use maxLatencyWriter to handle a direct Write() call.
	// Unlike Write() we know the ReadFrom() variant will properly call `mlw.stop()`.
	// Not calling stop will lead to flush-after-close and nil dereference panics.
	// This is common for non-proxy responses, such as io.WriteString in tests or
	// middleware sending a response body.
	http.ResponseWriter
	mlw maxLatencyWriter
}

// Implement io.ReaderFrom, this will allow goproxy to io.Copy to it
// with the flushing interval applied.
var _ io.ReaderFrom = (*maxLatencyResponseWriter)(nil)

func (f *maxLatencyResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	return f.mlw.ReadFrom(src)
}

var streamingBufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 32*1024)
	},
}
