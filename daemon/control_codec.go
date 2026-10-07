package daemon

import (
	"bufio"
	"encoding/gob"
	"io"
	"net"
	"net/rpc"
)

// gobServerCodec mirrors net/rpc's unexported gobServerCodec — ServeConn
// builds that one internally, so hooking reply completion (#5182) means
// supplying the codec ourselves through ServeCodec. The wire format is
// unchanged: gob response header, gob body, one buffered flush per response.
type gobServerCodec struct {
	rwc     io.ReadWriteCloser
	dec     *gob.Decoder
	enc     *gob.Encoder
	encBuf  *bufio.Writer
	pending *pendingUntracks
	closed  bool
}

func newGobServerCodec(conn net.Conn, pending *pendingUntracks) *gobServerCodec {
	encBuf := bufio.NewWriter(conn)
	return &gobServerCodec{
		rwc:     conn,
		dec:     gob.NewDecoder(conn),
		enc:     gob.NewEncoder(encBuf),
		encBuf:  encBuf,
		pending: pending,
	}
}

func (c *gobServerCodec) ReadRequestHeader(r *rpc.Request) error {
	return c.dec.Decode(r)
}

func (c *gobServerCodec) ReadRequestBody(body any) error {
	return c.dec.Decode(body)
}

// WriteResponse encodes the reply and flushes it into the socket — a nil
// return means this answer is already past us and cannot be lost on our side.
// Releasing the oldest parked unregister here, per reply, keeps each teardown
// requester registered for exactly the reply it is still waiting on rather
// than for the whole connection (Codex on #5186).
func (c *gobServerCodec) WriteResponse(r *rpc.Response, body any) error {
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
	if c.pending != nil {
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
	return c.rwc.Close()
}
