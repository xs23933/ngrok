package conn

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"ngrok/log"
)

type Conn interface {
	net.Conn
	log.Logger
	Id() string
	SetType(string)
}

type loggedConn struct {
	net.Conn
	log.Logger
	id  int32
	typ string
}

type Listener struct {
	net.Addr
	Conns chan Conn
}

func wrapConn(conn net.Conn, typ string) *loggedConn {
	c := &loggedConn{conn, log.NewPrefixLogger(), rand.Int31(), typ}
	c.AddLogPrefix(c.Id())
	return c
}

func Listen(addr, typ string, tlsCfg *tls.Config) (l *Listener, err error) {
	// listen for incoming connections
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}

	l = &Listener{
		Addr:  listener.Addr(),
		Conns: make(chan Conn),
	}

	go func() {
		for {
			rawConn, err := listener.Accept()
			if err != nil {
				log.Error("Failed to accept new TCP connection of type %s: %v", typ, err)
				continue
			}

			c := wrapConn(rawConn, typ)
			if tlsCfg != nil {
				c.Conn = tls.Server(c.Conn, tlsCfg)
			}
			c.Info("New connection from %v", c.RemoteAddr())
			l.Conns <- c
		}
	}()
	return
}

func Wrap(conn net.Conn, typ string) *loggedConn {
	return wrapConn(conn, typ)
}

func Dial(addr, typ string, tlsCfg *tls.Config) (conn *loggedConn, err error) {
	var rawConn net.Conn
	if rawConn, err = net.Dial("tcp", addr); err != nil {
		return
	}

	if tlsCfg != nil {
		rawConn = tls.Client(rawConn, tlsCfg)
	}

	conn = wrapConn(rawConn, typ)
	conn.Debug("New connection to: %v", rawConn.RemoteAddr())
	return
}

func (c *loggedConn) Close() error {
	c.Debug("Closing")
	return c.Conn.Close()
}

func (c *loggedConn) Id() string {
	return fmt.Sprintf("%s:%x", c.typ, c.id)
}

func (c *loggedConn) SetType(typ string) {
	oldId := c.Id()
	c.typ = typ
	c.ClearLogPrefixes()
	c.AddLogPrefix(c.Id())
	c.Info("Renamed connection %s", oldId)
}

func Join(c Conn, c2 Conn) (int64, int64) {
	done := make(chan error)
	pipe := func(to Conn, from Conn, bytesCopied *int64) {
		var err error
		*bytesCopied, err = io.Copy(to, from)
		if err != nil {
			from.Warn("Copied %d bytes to %s before failing with error %v", *bytesCopied, to.Id(), err)
			done <- err
		} else {
			from.Debug("Copied %d bytes from to %s", *bytesCopied, to.Id())
			done <- nil
		}
	}

	var fromBytes, toBytes int64
	go pipe(c, c2, &fromBytes)
	go pipe(c2, c, &toBytes)
	c.Info("Joined with connection %s", c2.Id())
	<-done
	c.Close()
	c2.Close()
	<-done
	return fromBytes, toBytes
}

type httpConn struct {
	*loggedConn
	reqBuf *bytes.Buffer
}

func NewHttp(conn net.Conn, typ string) *httpConn {
	return &httpConn{
		wrapConn(conn, typ),
		bytes.NewBuffer(make([]byte, 0, 1024)),
	}
}

func (c *httpConn) ReadRequest() (*http.Request, error) {
	r := io.TeeReader(c.loggedConn, c.reqBuf)
	return http.ReadRequest(bufio.NewReader(r))
}

func (c *loggedConn) ReadFrom(r io.Reader) (n int64, err error) {
	// special case when we're reading from an http request where
	// we had to parse the request and consume bytes from the socket
	// and store them in a temporary request buffer
	if httpConn, ok := r.(*httpConn); ok {
		if n, err = httpConn.reqBuf.WriteTo(c); err != nil {
			return
		}
	}

	nCopied, err := io.Copy(c.Conn, r)
	n += nCopied
	return
}
