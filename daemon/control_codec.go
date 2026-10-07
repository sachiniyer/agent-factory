package daemon

import (
	"bufio"
	"encoding/gob"
	"io"
	"net"
	"net/rpc"
	"sync"
)

// teardownServiceMethods are the net/rpc methods whose handlers may register a
// teardown requester (#5182) — every callsite of trackTeardownRequester on
// this service. A response written for anything else can never be the reply a
// parked unregister is waiting on, so it must not release one.
var teardownServiceMethods = map[string]bool{
	controlServiceName + ".KillSession":      true,
	controlServiceName + ".ArchiveSession":   true,
	controlServiceName + ".CloseTab":         true,
	controlServiceName + ".DeleteProject":    true,
	controlServiceName + ".ReapConfigAgent":  true,
	controlServiceName + ".ResumeFromLimit":  true,
	controlServiceName + ".HandoffSession":   true,
	controlServiceName + ".HandoffSessionV2": true,
}

// gobServerCodec mirrors net/rpc's unexported gobServerCodec — ServeConn
// builds that one internally, so hooking reply completion (#5182) means
// supplying the codec ourselves through ServeCodec. The wire format is
// unchanged: gob request/response header, gob body, one buffered flush per
// message.
type gobServerCodec struct {
	rwc     io.ReadWriteCloser
	dec     *gob.Decoder
	enc     *gob.Encoder
	encBuf  *bufio.Writer
	pending *pendingUntracks
	closed  bool

	// seqs records Seq -> ServiceMethod for every successfully read request
	// header. Reads happen in the serve loop while writes happen on
	// per-request send goroutines, so it needs its own mutex; entries are
	// consumed at WriteResponse, pruned at Close.
	seqMu sync.Mutex
	seqs  map[uint64]string
}

func newGobServerCodec(conn net.Conn, pending *pendingUntracks) *gobServerCodec {
	encBuf := bufio.NewWriter(conn)
	return &gobServerCodec{
		rwc:     conn,
		dec:     gob.NewDecoder(conn),
		enc:     gob.NewEncoder(encBuf),
		encBuf:  encBuf,
		pending: pending,
		seqs:    make(map[uint64]string),
	}
}

func (c *gobServerCodec) ReadRequestHeader(r *rpc.Request) error {
	if err := c.dec.Decode(r); err != nil {
		return err
	}
	c.seqMu.Lock()
	c.seqs[r.Seq] = r.ServiceMethod
	c.seqMu.Unlock()
	return nil
}

func (c *gobServerCodec) ReadRequestBody(body any) error {
	return c.dec.Decode(body)
}

// WriteResponse encodes the reply and flushes it into the socket — a nil
// return means this answer is already past us and cannot be lost on our side.
// Only a response to a method that can register a teardown requester releases
// a parked unregister: when a connection multiplexes a teardown call with an
// ordinary one, the ordinary reply winning the send lock must not free an
// exemption its own call is still queued behind (Codex on #5186). Unregisters
// are fungible decrements of the same (pid, start-stamp) refcount, so among
// teardown replies FIFO pop is the count that matters.
func (c *gobServerCodec) WriteResponse(r *rpc.Response, body any) error {
	c.seqMu.Lock()
	method, known := c.seqs[r.Seq]
	delete(c.seqs, r.Seq)
	c.seqMu.Unlock()
	if err := c.enc.Encode(r); err != nil {
		_ = c.encBuf.Flush()
		return err
	}
	if err := c.enc.Encode(body); err != nil {
		_ = c.encBuf.Flush()
		return err
	}
	if err := c.encBuf.Flush(); err != nil {
		return err
	}
	if known && teardownServiceMethods[method] && c.pending != nil {
		c.pending.pop()
	}
	return nil
}

func (c *gobServerCodec) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	// The connection is dead: anything still parked belongs to a reply the
	// client will never read — release it rather than exempt the requester
	// forever (#5182).
	if c.pending != nil {
		c.pending.drain()
	}
	c.seqMu.Lock()
	c.seqs = nil
	c.seqMu.Unlock()
	return c.rwc.Close()
}
