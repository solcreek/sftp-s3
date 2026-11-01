package gateway

import (
	"bufio"
	"net"

	"github.com/pires/go-proxyproto"
)

// readProxyHeader reads a PROXY protocol header (v1 or v2) from the start
// of nc and returns a connection that reports the client address it
// carries. The caller's deadline on nc bounds the read.
//
// A LOCAL header (a health check by the proxy itself) carries no client, so
// the proxy's own address is kept.
func readProxyHeader(nc net.Conn) (net.Conn, error) {
	br := bufio.NewReader(nc)
	// A probe that closes without sending anything should surface as EOF
	// (logged quietly, like a health check), not as a missing signature.
	if _, err := br.Peek(1); err != nil {
		return nil, err
	}
	h, err := proxyproto.Read(br)
	if err != nil {
		return nil, err
	}
	pc := &proxiedConn{Conn: nc, r: br, remote: nc.RemoteAddr()}
	if src, _, ok := h.TCPAddrs(); ok && h.Command.IsProxy() {
		pc.remote = src
	}
	return pc, nil
}

// proxiedConn reads through the buffer that held the header, so no bytes
// after it are lost, and reports the client's address.
type proxiedConn struct {
	net.Conn
	r      *bufio.Reader
	remote net.Addr
}

func (c *proxiedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *proxiedConn) RemoteAddr() net.Addr       { return c.remote }
