// Copyright 2013-2015 go-diameter authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Diameter server, based on net/http.

package diam

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"runtime"
	"sync"
	"time"

	"golang.org/x/net/context"

	"github.com/fiorix/go-diameter/diam/dict"
)

// The Handler interface allow arbitrary objects to be
// registered to serve particular messages like CER, DWR.
//
// ServeDIAM should write messages to the Conn and then return.
// Returning signals that the request is finished and that the
// server can move on to the next request on the connection.
type Handler interface {
	ServeDIAM(Conn, *Message)
}

// Conn interface is used by a handler to send diameter messages.
type Conn interface {
	Write(b []byte) (int, error)    // Writes a msg to the connection
	Close()                         // Close the connection
	LocalAddr() net.Addr            // Returns the local IP
	RemoteAddr() net.Addr           // Returns the remote IP
	TLS() *tls.ConnectionState      // TLS or nil when not using TLS
	Context() context.Context       // Returns the internal context
	SetContext(ctx context.Context) // Stores a new context
}

// The CloseNotifier interface is implemented by Conns which
// allow detecting when the underlying connection has gone away.
//
// This mechanism can be used to detect if a peer has disconnected.
type CloseNotifier interface {
	// CloseNotify returns a channel that is closed
	// when the client connection has gone away.
	CloseNotify() <-chan struct{}
}

// A liveSwitchReader is a switchReader that's safe for concurrent
// reads and switches, if its mutex is held.
type liveSwitchReader struct {
	sync.Mutex
	r io.Reader
}

func (sr *liveSwitchReader) Read(p []byte) (n int, err error) {
	sr.Lock()
	r := sr.r
	sr.Unlock()
	return r.Read(p)
}

// conn represents the server side of a diameter connection.
type conn struct {
	server   *Server              // the Server on which the connection arrived
	rwc      net.Conn             // i/o connection
	sr       liveSwitchReader     // reads from rwc
	buf      *bufio.ReadWriter    // buffered(sr, rwc)
	tlsState *tls.ConnectionState // or nil when not using TLS
	writer   *response            // the diam.Conn exposed to handlers

	mu           sync.Mutex // guards the following
	closeNotifyc chan struct{}
	clientGone   bool
}

func (c *conn) closeNotify() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeNotifyc == nil {
		c.closeNotifyc = make(chan struct{})
		pr, pw := io.Pipe()
		readSource := c.sr.r
		c.sr.Lock()
		c.sr.r = pr
		c.sr.Unlock()
		go func() {
			_, err := io.Copy(pw, readSource)
			if err == nil {
				err = io.EOF
			}
			pw.CloseWithError(err)
			c.notifyClientGone()
		}()
	}
	return c.closeNotifyc
}

func (c *conn) notifyClientGone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeNotifyc != nil && !c.clientGone {
		close(c.closeNotifyc) // unblock readers
		c.clientGone = true
	}
}

// Create new connection from rwc.
func (srv *Server) newConn(rwc net.Conn) (c *conn, err error) {
	c = &conn{
		server: srv,
		rwc:    rwc,
		sr:     liveSwitchReader{r: rwc},
	}
	c.buf = bufio.NewReadWriter(bufio.NewReader(&c.sr), bufio.NewWriter(rwc))
	c.writer = &response{conn: c}
	return c, nil
}

// Read next message from connection.
func (c *conn) readMessage() (*Message, error) {
	dp := c.server.Dict
	if dp == nil {
		dp = dict.Default
	}
	if c.server.ReadTimeout > 0 {
		c.rwc.SetReadDeadline(time.Now().Add(c.server.ReadTimeout))
	}
	m, err := ReadMessage(c.buf.Reader, dp)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Serve a new connection.
func (c *conn) serve() {
	defer func() {
		if err := recover(); err != nil {
			buf := make([]byte, 4096)
			buf = buf[:runtime.Stack(buf, false)]
			log.Printf("DIAM: panic serving %v: %v\n%s",
				c.rwc.RemoteAddr().String(), err, buf)
		}
		c.rwc.Close()
	}()
	if tlsConn, ok := c.rwc.(*tls.Conn); ok {
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		c.tlsState = &tls.ConnectionState{}
		*c.tlsState = tlsConn.ConnectionState()
	}
	for {
		m, err := c.readMessage()
		if err != nil {
			c.rwc.Close()
			// Report errors to the channel, except EOF.
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				h := c.server.Handler
				if h == nil {
					h = DefaultServeMux
				}
				if er, ok := h.(ErrorReporter); ok {
					er.Error(ErrorReport{c.writer, m, err})
				}
			}
			break
		}
		// Handle messages in this goroutine.
		serverHandler{c.server}.ServeDIAM(c.writer, m)
	}
}

// A response represents the server side of a diameter response.
// It implements the Conn, CloseNotifier and Contexter interfaces.
type response struct {
	conn *conn           // socket, reader and writer
	mu   sync.Mutex      // guards ctx and Write
	ctx  context.Context // context for this Conn
}

// Write writes the message m to the connection.
func (w *response) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn.server.WriteTimeout > 0 {
		w.conn.rwc.SetWriteDeadline(time.Now().Add(w.conn.server.WriteTimeout))
	}
	n, err := w.conn.buf.Writer.Write(b)
	if err != nil {
		return 0, err
	}
	if err = w.conn.buf.Writer.Flush(); err != nil {
		return 0, err
	}
	return n, nil
}

// Close closes the connection.
func (w *response) Close() {
	w.conn.rwc.Close()
}

// LocalAddr returns the local address of the connection.
func (w *response) LocalAddr() net.Addr {
	return w.conn.rwc.LocalAddr()
}

// RemoteAddr returns the peer address of the connection.
func (w *response) RemoteAddr() net.Addr {
	return w.conn.rwc.RemoteAddr()
}

// TLS returns the TLS connection state, or nil.
func (w *response) TLS() *tls.ConnectionState {
	return w.conn.tlsState
}

// CloseNotify implements the CloseNotifier interface.
func (w *response) CloseNotify() <-chan struct{} {
	return w.conn.closeNotify()
}

// Context returns the internal context or a new context.Background.
func (w *response) Context() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx == nil {
		w.ctx = context.Background()
	}
	return w.ctx
}

// SetContext replaces the internal context with the given one.
func (w *response) SetContext(ctx context.Context) {
	w.mu.Lock()
	w.ctx = ctx
	w.mu.Unlock()
}

// The HandlerFunc type is an adapter to allow the use of
// ordinary functions as diameter handlers.  If f is a function
// with the appropriate signature, HandlerFunc(f) is a
// Handler object that calls f.
type HandlerFunc func(Conn, *Message)

// ServeDIAM calls f(c, m).
func (f HandlerFunc) ServeDIAM(c Conn, m *Message) {
	f(c, m)
}

// The ErrorReporter interface is implemented by Handlers that
// allow reading errors from the underlying connection, like
// parsing diameter messages or connection errors.
type ErrorReporter interface {
	// Error writes an error to the reporter.
	Error(err ErrorReport)

	// ErrorReports returns a channel that receives
	// errors from the connection.
	ErrorReports() <-chan ErrorReport
}

// ErrorReport is sent out of the server in case it fails to
// read messages due to a bad dictionary or network errors.
type ErrorReport struct {
	Conn    Conn     // Peer that caused the error
	Message *Message // Message that caused the error
	Error   error    // Error message
}

// ServeMux is a diameter message multiplexer. It matches the
// command from the incoming message against a list of
// registered commands and calls the handler.
type ServeMux struct {
	e  chan ErrorReport
	mu sync.RWMutex // Guards m.
	m  map[string]muxEntry
}

type muxEntry struct {
	h   Handler
	cmd string
}

// NewServeMux allocates and returns a new ServeMux.
func NewServeMux() *ServeMux {
	return &ServeMux{
		e: make(chan ErrorReport, 1),
		m: make(map[string]muxEntry),
	}
}

// DefaultServeMux is the default ServeMux used by Serve.
var DefaultServeMux = NewServeMux()

// Error implements the ErrorReporter interface.
func (mux *ServeMux) Error(err ErrorReport) {
	select {
	case mux.e <- err:
	default:
	}
}

// ErrorReports implement the ErrorReporter interface.
func (mux *ServeMux) ErrorReports() <-chan ErrorReport {
	return mux.e
}

// ServeDIAM dispatches the request to the handler that match the code
// in the incoming message. If the special "ALL" handler is registered
// it is used as a catch-all. Otherwise an ErrorReport is sent out.
func (mux *ServeMux) ServeDIAM(c Conn, m *Message) {
	mux.mu.RLock()
	defer mux.mu.RUnlock()
	dcmd, err := m.Dictionary().FindCommand(
		m.Header.ApplicationID,
		m.Header.CommandCode,
	)
	if err != nil {
		// Try the catch-all.
		mux.serve("ALL", c, m)
		return
	}
	var cmd string
	if m.Header.CommandFlags&RequestFlag == RequestFlag {
		cmd = dcmd.Short + "R"
	} else {
		cmd = dcmd.Short + "A"
	}
	mux.serve(cmd, c, m)
}

func (mux *ServeMux) serve(cmd string, c Conn, m *Message) {
	entry, ok := mux.m[cmd]
	if ok {
		entry.h.ServeDIAM(c, m)
		return
	}
	mux.Error(ErrorReport{
		Conn:    c,
		Message: m,
		Error:   errors.New("unhandled message"),
	})
}

// Handle registers the handler for the given code.
// If a handler already exists for code, Handle panics.
func (mux *ServeMux) Handle(cmd string, handler Handler) {
	mux.mu.Lock()
	defer mux.mu.Unlock()
	if handler == nil {
		panic("DIAM: nil handler")
	}
	mux.m[cmd] = muxEntry{h: handler, cmd: cmd}
}

// HandleFunc registers the handler function for the given command.
// Special cmd "ALL" may be used as a catch all.
func (mux *ServeMux) HandleFunc(cmd string, handler func(Conn, *Message)) {
	mux.Handle(cmd, HandlerFunc(handler))
}

// Handle registers the handler object for the given command
// in the DefaultServeMux.
func Handle(cmd string, handler Handler) {
	DefaultServeMux.Handle(cmd, handler)
}

// HandleFunc registers the handler function for the given command
// in the DefaultServeMux.
func HandleFunc(cmd string, handler func(Conn, *Message)) {
	DefaultServeMux.HandleFunc(cmd, handler)
}

// ErrorReports returns the ErrorReport channel of the DefaultServeMux.
func ErrorReports() <-chan ErrorReport {
	return DefaultServeMux.ErrorReports()
}

// Serve accepts incoming diameter connections on the listener l,
// creating a new service goroutine for each.  The service goroutines
// read messages and then call handler to reply to them.
// Handler is typically nil, in which case the DefaultServeMux is used.
func Serve(l net.Listener, handler Handler) error {
	srv := &Server{Handler: handler}
	return srv.Serve(l)
}

// A Server defines parameters for running a diameter server.
type Server struct {
	Addr         string        // TCP address to listen on, ":3868" if empty
	Handler      Handler       // handler to invoke, DefaultServeMux if nil
	Dict         *dict.Parser  // diameter dictionaries for this server
	ReadTimeout  time.Duration // maximum duration before timing out read of the request
	WriteTimeout time.Duration // maximum duration before timing out write of the response
	TLSConfig    *tls.Config   // optional TLS config, used by ListenAndServeTLS
}

// serverHandler delegates to either the server's Handler or DefaultServeMux.
type serverHandler struct {
	srv *Server
}

func (sh serverHandler) ServeDIAM(w Conn, m *Message) {
	handler := sh.srv.Handler
	if handler == nil {
		handler = DefaultServeMux
	}
	handler.ServeDIAM(w, m)
}

// ListenAndServe listens on the TCP network address srv.Addr and then
// calls Serve to handle requests on incoming connections.  If
// srv.Addr is blank, ":3868" is used.
func (srv *Server) ListenAndServe() error {
	addr := srv.Addr
	if len(addr) == 0 {
		addr = ":3868"
	}
	l, e := net.Listen("tcp", addr)
	if e != nil {
		return e
	}
	return srv.Serve(l)
}

// Serve accepts incoming connections on the Listener l, creating a
// new service goroutine for each.  The service goroutines read requests and
// then call srv.Handler to reply to them.
func (srv *Server) Serve(l net.Listener) error {
	defer l.Close()
	var tempDelay time.Duration // how long to sleep on accept failure
	for {
		rw, e := l.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				log.Printf("DIAM: Accept error: %v; retrying in %v", e, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return e
		}
		tempDelay = 0
		if c, err := srv.newConn(rw); err != nil {
			continue
		} else {
			go c.serve()
		}
	}
}

// ListenAndServe listens on the TCP network address addr
// and then calls Serve with handler to handle requests
// on incoming connections.
//
// If handler is nil, DefaultServeMux is used.
//
// If dict is nil, dict.Default is used.
func ListenAndServe(addr string, handler Handler, dp *dict.Parser) error {
	server := &Server{Addr: addr, Handler: handler, Dict: dp}
	return server.ListenAndServe()
}

// ListenAndServeTLS listens on the TCP network address srv.Addr and
// then calls Serve to handle requests on incoming TLS connections.
//
// Filenames containing a certificate and matching private key for
// the server must be provided. If the certificate is signed by a
// certificate authority, the certFile should be the concatenation
// of the server's certificate followed by the CA's certificate.
//
// If srv.Addr is blank, ":3868" is used.
func (srv *Server) ListenAndServeTLS(certFile, keyFile string) error {
	addr := srv.Addr
	if len(addr) == 0 {
		addr = ":3868"
	}
	config := &tls.Config{}
	if srv.TLSConfig != nil {
		*config = *srv.TLSConfig
	}
	var err error
	config.Certificates = make([]tls.Certificate, 1)
	config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	tlsListener := tls.NewListener(conn, config)
	return srv.Serve(tlsListener)
}

// ListenAndServeTLS acts identically to ListenAndServe, except that it
// expects SSL connections. Additionally, files containing a certificate and
// matching private key for the server must be provided. If the certificate
// is signed by a certificate authority, the certFile should be the concatenation
// of the server's certificate followed by the CA's certificate.
//
// One can use generate_cert.go in crypto/tls to generate cert.pem and key.pem.
func ListenAndServeTLS(addr string, certFile string, keyFile string, handler Handler, dp *dict.Parser) error {
	server := &Server{Addr: addr, Handler: handler, Dict: dp}
	return server.ListenAndServeTLS(certFile, keyFile)
}
