package daemon

import (
	"bufio"
	"encoding/gob"
	"io"
	"net"
	"net/rpc"
	"sync"
)

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

	// curSeq is the Seq of the most recently read request header. Reads are
	// serialized on the connection's single read loop, so it needs no mutex.
	// argvBySeq maps that Seq to the argv pointer net/rpc decoded the request
	// into — the same pointer the service method receives as req and teardown
	// handlers park their unregister under (trackTeardownRequester).
	// WriteResponse consumes it, so an errored response — where sendResponse
	// replaces the handler's reply with the invalidRequest sentinel — still
	// finds and releases exactly its own call's registration (#5186).
	curSeq    uint64
	seqMu     sync.Mutex
	argvBySeq map[uint64]any
}

func newGobServerCodec(conn net.Conn, pending *pendingUntracks) *gobServerCodec {
	encBuf := bufio.NewWriter(conn)
	return &gobServerCodec{
		rwc:       conn,
		dec:       gob.NewDecoder(conn),
		enc:       gob.NewEncoder(encBuf),
		encBuf:    encBuf,
		pending:   pending,
		argvBySeq: make(map[uint64]any),
	}
}

func (c *gobServerCodec) ReadRequestHeader(r *rpc.Request) error {
	if err := c.dec.Decode(r); err != nil {
		return err
	}
	c.curSeq = r.Seq
	return nil
}

func (c *gobServerCodec) ReadRequestBody(body any) error {
	if err := c.dec.Decode(body); err != nil {
		return err
	}
	if body != nil {
		c.seqMu.Lock()
		c.argvBySeq[c.curSeq] = body
		c.seqMu.Unlock()
	}
	return nil
}

// WriteResponse encodes the reply and flushes it into the socket — a nil
// return means this answer is already past us and cannot be lost on our side.
// The request's argv pointer — recorded at ReadRequestBody — is what the
// teardown handler parked its unregister under, so releasing by it frees the
// exemption of the call this response answers and no other, on success and on
// handler error alike. A multiplexed Ping reply, a request that never reached
// a handler (unreadable body, unknown method — net/rpc answers those without
// dispatching), or any non-teardown response simply finds no parked entry
// (#5182, Codex on #5186).
func (c *gobServerCodec) WriteResponse(r *rpc.Response, body any) error {
	c.seqMu.Lock()
	argv, registered := c.argvBySeq[r.Seq]
	if registered {
		delete(c.argvBySeq, r.Seq)
	}
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
	if registered && c.pending != nil {
		c.pending.releaseFor(argv)
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
	c.argvBySeq = nil
	c.seqMu.Unlock()
	return c.rwc.Close()
}
