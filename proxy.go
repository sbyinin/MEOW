package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/cyfdecyf/bufio"
	"github.com/cyfdecyf/leakybuf"
	ss "github.com/shadowsocks/shadowsocks-go/shadowsocks"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// As I'm using ReadSlice to read line, it's possible to get
// bufio.ErrBufferFull while reading line, so set it to a large value to
// prevent such problems.
//
// For limits about URL and HTTP header size, refer to:
// http://stackoverflow.com/questions/417142/what-is-the-maximum-length-of-a-url
// "de facto limit of 2000 characters"
// http://www.mnot.net/blog/2011/07/11/what_proxies_must_do
// "URIs should be allowed at least 8000 octets, and HTTP headers should have
// 4000 as an absolute minimum".
// In practice, there are sites using cookies larger than 4096 bytes,
// e.g. www.fitbit.com. So set http buffer size to 8192 to be safe.
const httpBufSize = 8192

// Hold at most 4MB memory as buffer for parsing http request/response and
// holding post data.
var httpBuf = leakybuf.NewLeakyBuf(512, httpBufSize)

// If no keep-alive header in response, use this as the keep-alive value.
const defaultServerConnTimeout = 5 * time.Second

// Close client connection if no new requests received in some time.
// (On OS X, the default soft limit of open file descriptor is 256, which is
// very conservative and easy to cause problem if we are not careful to limit
// open fds.)
const clientConnTimeout = 5 * time.Second
const fullKeepAliveHeader = "Keep-Alive: timeout=5\r\n"

// Some code are learnt from the http package

var zeroTime time.Time

type directConn struct {
	net.Conn
}

func (dc directConn) String() string {
	return "direct connection"
}

type serverConnState byte

const (
	svConnected serverConnState = iota
	svSendRecvResponse
	svStopped
)

type serverConn struct {
	net.Conn
	bufRd       *bufio.Reader
	buf         []byte // buffer for the buffered reader
	hostPort    string
	state       serverConnState
	willCloseOn time.Time
	direct      bool
}

type clientConn struct {
	net.Conn // connection to the proxy client
	bufRd    *bufio.Reader
	buf      []byte // buffer for the buffered reader
	proxy    Proxy
}

var (
	errPageSent      = errors.New("error page has sent")
	errClientTimeout = errors.New("read client request timeout")
	errAuthRequired  = errors.New("authentication requried")
)

type Proxy interface {
	Serve(*sync.WaitGroup)
	Addr() string
	genConfig() string // for upgrading config
}

var listenProxy []Proxy

func addListenProxy(p Proxy) {
	listenProxy = append(listenProxy, p)
}

type httpProxy struct {
	addr      string // listen address, contains port
	port      string // for use when generating PAC
	addrInPAC string // proxy server address to use in PAC
}

func newHttpProxy(addr, addrInPAC string) *httpProxy {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		panic("proxy addr" + err.Error())
	}
	return &httpProxy{addr, port, addrInPAC}
}

func (proxy *httpProxy) genConfig() string {
	if proxy.addrInPAC != "" {
		return fmt.Sprintf("listen = http://%s %s", proxy.addr, proxy.addrInPAC)
	} else {
		return fmt.Sprintf("listen = http://%s", proxy.addr)
	}
}

func (proxy *httpProxy) Addr() string {
	return proxy.addr
}

func (hp *httpProxy) Serve(wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
	}()
	ln, err := net.Listen("tcp", hp.addr)
	if err != nil {
		fmt.Println("listen http failed:", err)
		return
	}
	host, _, _ := net.SplitHostPort(hp.addr)
	var pacURL string
	if host == "" || host == "0.0.0.0" {
		pacURL = fmt.Sprintf("http://<hostip>:%s/pac", hp.port)
	} else if hp.addrInPAC == "" {
		pacURL = fmt.Sprintf("http://%s/pac", hp.addr)
	} else {
		pacURL = fmt.Sprintf("http://%s/pac", hp.addrInPAC)
	}
	info.Printf("listen http %s, PAC url %s\n", hp.addr, pacURL)

	for {
		conn, err := ln.Accept()
		if err != nil {
			errl.Printf("http proxy(%s) accept %v\n", ln.Addr(), err)
			if isErrTooManyOpenFd(err) {
				connPool.CloseAll()
			}
			time.Sleep(time.Millisecond)
			continue
		}
		c := newClientConn(conn, hp)
		go c.serve()
	}
}

type meowProxy struct {
	addr   string
	method string
	passwd string
	cipher *ss.Cipher
}

func newMeowProxy(method, passwd, addr string) *meowProxy {
	cipher, err := ss.NewCipher(method, passwd)
	if err != nil {
		Fatal("can't initialize meow proxy server", err)
	}
	return &meowProxy{addr, method, passwd, cipher}
}

func (cp *meowProxy) genConfig() string {
	method := cp.method
	if method == "" {
		method = "table"
	}
	return fmt.Sprintf("listen = meow://%s:%s@%s", method, cp.passwd, cp.addr)
}

func (cp *meowProxy) Addr() string {
	return cp.addr
}

func (cp *meowProxy) Serve(wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
	}()
	ln, err := net.Listen("tcp", cp.addr)
	if err != nil {
		fmt.Println("listen meow failed:", err)
		return
	}
	info.Printf("meow proxy address %s\n", cp.addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			errl.Printf("meow proxy(%s) accept %v\n", ln.Addr(), err)
			if isErrTooManyOpenFd(err) {
				connPool.CloseAll()
			}
			time.Sleep(time.Millisecond)
			continue
		}
		ssConn := ss.NewConn(conn, cp.cipher.Copy())
		c := newClientConn(ssConn, cp)
		go c.serve()
	}
}

func newClientConn(cli net.Conn, proxy Proxy) *clientConn {
	buf := httpBuf.Get()
	c := &clientConn{
		Conn:  cli,
		buf:   buf,
		bufRd: bufio.NewReaderFromBuf(cli, buf),
		proxy: proxy,
	}
	if debug {
		debug.Printf("cli(%s) connected, total %d clients\n",
			cli.RemoteAddr(), incCliCnt())
	}
	return c
}

func (c *clientConn) releaseBuf() {
	if c.bufRd != nil {
		// debug.Println("release client buffer")
		httpBuf.Put(c.buf)
		c.buf = nil
		c.bufRd = nil
	}
}

func (c *clientConn) Close() {
	c.releaseBuf()
	if debug {
		debug.Printf("cli(%s) closed, total %d clients\n",
			c.RemoteAddr(), decCliCnt())
	}
	c.Conn.Close()
}

// Listen address as key, not including port part.
var selfListenAddr map[string]bool

// Called in main, so no need to protect concurrent initialization.
func initSelfListenAddr() {
	selfListenAddr = make(map[string]bool)
	// Add empty host to self listen addr, in case there's no Host header.
	selfListenAddr[""] = true
	for _, proxy := range listenProxy {
		addr := proxy.Addr()
		// Handle wildcard address.
		if addr[0] == ':' || strings.HasPrefix(addr, "0.0.0.0") {
			for _, ad := range hostAddr() {
				selfListenAddr[ad] = true
			}
			selfListenAddr["localhost"] = true
			selfListenAddr["127.0.0.1"] = true
			continue
		}

		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			panic("listen addr invalid: " + addr)
		}
		selfListenAddr[host] = true
		if host == "127.0.0.1" {
			selfListenAddr["localhost"] = true
		} else if host == "localhost" {
			selfListenAddr["127.0.0.1"] = true
		}
	}
}

func isSelfRequest(r *Request) bool {
	if r.URL.HostPort != "" {
		return false
	}
	// Maxthon sometimes sends requests without host in request line,
	// in that case, get host information from Host header.
	// But if client PAC setting is using meow server's DNS name, we can't
	// decide if the request is for meow itself (need reverse lookup).
	// So if request path seems like getting PAC, simply return true.
	if r.URL.Path == "/pac" || strings.HasPrefix(r.URL.Path, "/pac?") {
		return true
	}
	r.URL.ParseHostPort(r.Header.Host)
	if selfListenAddr[r.URL.Host] {
		return true
	}
	debug.Printf("fixed request with no host in request line %s\n", r)
	return false
}

func (c *clientConn) serveSelfURL(r *Request) (err error) {
	if _, ok := c.proxy.(*httpProxy); !ok {
		goto end
	}
	if r.Method != "GET" {
		goto end
	}
	if r.URL.Path == "/pac" || strings.HasPrefix(r.URL.Path, "/pac?") {
		sendPAC(c)
		// PAC header contains connection close, send non nil error to close
		// client connection.
		return errPageSent
	}
end:
	sendErrorPage(c, "404 not found", "Page not found",
		genErrMsg(r, nil, "Serving request to meow proxy."))
	errl.Printf("cli(%s) page not found, serving request to meow %s\n%s",
		c.RemoteAddr(), r, r.Verbose())
	return errPageSent
}

func dbgPrintRq(c *clientConn, r *Request, direct bool) {
	if r.Trailer {
		errl.Printf("cli(%s) request  %s has Trailer header\n%s",
			c.RemoteAddr(), r, r.Verbose())
	}
	if dbgRq {
		var connType string
		if direct {
			connType = "DIRECT"
		} else {
			connType = "PROXY"
		}
		if verbose {
			dbgRq.Printf("%s %s %s\n%s\n", connType, c.RemoteAddr(), r, r.Verbose())
		} else {
			dbgRq.Printf("%s %s %s\n", connType, c.RemoteAddr(), r)
		}
	}
}

type SinkWriter struct{}

func (s SinkWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (c *clientConn) serve() {
	var r Request
	var rp Response
	var sv *serverConn
	var err error

	var authed bool
	// For meow proxy server, authentication is done by matching password.
	if _, ok := c.proxy.(*meowProxy); ok {
		authed = true
	}

	defer func() {
		r.releaseBuf()
		c.Close()
	}()

	// Refer to implementation.md for the design choices on parsing the request
	// and response.
	for {
		if c.bufRd == nil || c.buf == nil {
			panic("client read buffer nil")
		}

		if err = parseRequest(c, &r); err != nil {
			debug.Printf("cli(%s) parse request %v\n", c.RemoteAddr(), err)
			if err == io.EOF || isErrConnReset(err) {
				return
			}
			if err != errClientTimeout {
				sendErrorPage(c, "404 Bad request", "Bad request", err.Error())
				return
			}
			sendErrorPage(c, statusRequestTimeout, statusRequestTimeout,
				"Your browser didn't send a complete request in time.")
			return
		}

		// PAC may leak frequently visited sites information. But if meow
		// requires authentication for PAC, some clients may not be able
		// handle it. (e.g. Proxy SwitchySharp extension on Chrome.)
		if isSelfRequest(&r) {
			if err = c.serveSelfURL(&r); err != nil {
				return
			}
			continue
		}

		if auth.required && !authed {
			if err = Authenticate(c, &r); err != nil {
				errl.Printf("cli(%s) %v\n", c.RemoteAddr(), err)
				// Request may have body. To make things simple, close
				// connection so we don't need to skip request body before
				// reading the next request.
				return
			}
			authed = true
		}

		if r.ExpectContinue {
			sendErrorPage(c, statusExpectFailed, "Expect header not supported",
				"Please contact meow's developer if you see this.")
			// Client may have sent request body at this point. Simply close
			// connection so we don't need to handle this case.
			// NOTE: sendErrorPage tells client the connection will keep alive, but
			// actually it will close here.
			return
		}

		if sv, err = c.getServerConn(&r); err != nil {
			if debug {
				debug.Printf("cli(%s) failed to get server conn %v\n", c.RemoteAddr(), &r)
			}
			// Failed connection will send error page back to the client.
			// For CONNECT, the client read buffer is released in copyClient2Server,
			// so can't go back to getRequest.
			if err == errPageSent && !r.isConnect {
				if r.hasBody() {
					// skip request body
					debug.Printf("cli(%s) skip request body %v\n", c.RemoteAddr(), &r)
					sendBody(SinkWriter{}, c.bufRd, int(r.ContLen), r.Chunking)
				}
				continue
			}
			return
		}

		if r.isConnect {
			// server connection will be closed in doConnect
			err = sv.doConnect(&r, c)
			// debug.Printf("doConnect %s to %s done\n", c.RemoteAddr(), r.URL.HostPort)
			return
		}

		if err = sv.doRequest(c, &r, &rp); err != nil {
			// For client I/O error, we can actually put server connection to
			// pool. But let's make thing simple for now.
			sv.Close()
			if err == errPageSent && (!r.hasBody() || r.hasSent()) {
				// Can only continue if request has no body, or request body
				// has been read.
				continue
			}
			return
		}
		// Put server connection to pool, so other clients can use it.
		_, isMeowConn := sv.Conn.(meowConn)
		if rp.ConnectionKeepAlive || isMeowConn {
			if debug {
				debug.Printf("cli(%s) connPool put %s", c.RemoteAddr(), sv.hostPort)
			}
			// If the server connection is not going to be used soon,
			// release buffer before putting back to pool can save memory.
			sv.releaseBuf()
			connPool.Put(sv)
		} else {
			if debug {
				debug.Printf("cli(%s) server %s close conn\n", c.RemoteAddr(), sv.hostPort)
			}
			sv.Close()
		}

		if !r.ConnectionKeepAlive {
			if debug {
				debug.Printf("cli(%s) close connection\n", c.RemoteAddr())
			}
			return
		}
	}
}

func genErrMsg(r *Request, sv *serverConn, what string) string {
	if sv == nil {
		return fmt.Sprintf("<p>HTTP Request <strong>%v</strong></p> <p>%s</p>", r, what)
	}
	return fmt.Sprintf("<p>HTTP Request <strong>%v</strong></p> <p>%s</p> <p>Using %s.</p>",
		r, what, sv.Conn)
}

func (c *clientConn) handleServerReadError(r *Request, sv *serverConn, err error, msg string) error {
	if debug {
		debug.Printf("cli(%s) server read error %s %T %v %v\n",
			c.RemoteAddr(), msg, err, err, r)
	}
	if err == io.EOF {
		return err
	}
	if isErrTimeout(err) || isErrConnReset(err) || isHttpErrCode(err) {
		return err
	}
	if r.responseNotSent() {
		sendErrorPage(c, "502 read error", err.Error(), genErrMsg(r, sv, msg))
		return errPageSent
	}
	errl.Printf("cli(%s) unhandled server read error %s %v %s\n", c.RemoteAddr(), msg, err, r)
	return err
}

func (c *clientConn) handleServerWriteError(r *Request, sv *serverConn, err error, msg string) error {
	return err
}

func dbgPrintRep(c *clientConn, r *Request, rp *Response) {
	if rp.Trailer {
		errl.Printf("cli(%s) response %s has Trailer header\n%s",
			c.RemoteAddr(), rp, rp.Verbose())
	}
	if dbgRep {
		if verbose {
			dbgRep.Printf("%s %s\n%s", r, rp, rp.Verbose())
		} else {
			dbgRep.Printf("%s %s\n", r, rp)
		}
	}
}

func (c *clientConn) readResponse(sv *serverConn, r *Request, rp *Response) (err error) {
	sv.initBuf()
	defer func() {
		rp.releaseBuf()
	}()

	/*
		if r.partial {
			return RetryError{errors.New("debug retry for partial request")}
		}
	*/

	/*
		// force retry for debugging
		if r.tryCnt == 1 {
			return RetryError{errors.New("debug retry in readResponse")}
		}
	*/

	if err = parseResponse(sv, r, rp); err != nil {
		return c.handleServerReadError(r, sv, err, "parse response")
	}
	dbgPrintRep(c, r, rp)
	// After have received the first reponses from the server, we consider
	// ther server as real instead of fake one caused by wrong DNS reply. So
	// don't time out later.
	sv.state = svSendRecvResponse
	r.state = rsRecvBody
	r.releaseBuf()

	if _, err = c.Write(rp.rawResponse()); err != nil {
		return err
	}

	rp.releaseBuf()

	if rp.hasBody(r.Method) {
		if err = sendBody(c, sv.bufRd, int(rp.ContLen), rp.Chunking); err != nil {
			if debug {
				debug.Printf("cli(%s) send body %v\n", c.RemoteAddr(), err)
			}
			// Non persistent connection will return nil upon successful response reading
			if err == io.EOF {
				// For persistent connection, EOF from server is error.
				// Response header has been read, server using persistent
				// connection indicates the end of response and proxy should
				// not got EOF while reading response.
				// The client connection will be closed to indicate this error.
				// Proxy can't send error page here because response header has
				// been sent.
				return fmt.Errorf("read response body unexpected EOF %v", rp)
			} else if isErrOpRead(err) {
				return c.handleServerReadError(r, sv, err, "read response body")
			}
			// errl.Printf("cli(%s) sendBody error %T %v %v", err, err, r)
			return err
		}
	}
	r.state = rsDone
	
		if debug {
			debug.Printf("[Finished] %s \n", r)
			debug.Printf("[Finished] %v request %s %s\n", c.RemoteAddr(), r.Method, r.URL)
		}
	
	if rp.ConnectionKeepAlive {
		if rp.KeepAlive == time.Duration(0) {
			sv.willCloseOn = time.Now().Add(defaultServerConnTimeout)
		} else {
			// debug.Printf("cli(%s) server %s keep-alive %v\n", c.RemoteAddr(), sv.hostPort, rp.KeepAlive)
			sv.willCloseOn = time.Now().Add(rp.KeepAlive)
		}
	}
	return
}

func (c *clientConn) getServerConn(r *Request) (*serverConn, error) {
	direct := directList.shouldDirect(r.URL)
	// For CONNECT method, always create new connection.
	if r.isConnect {
		return c.createServerConn(r, direct)
	}
	sv := connPool.Get(r.URL.HostPort, direct)
	if sv != nil {
		// For websites like feedly, the site itself is not blocked, but the
		// content it loads may result reset. So we should reset server
		// connection state to just connected.
		sv.state = svConnected
		if debug {
			debug.Printf("cli(%s) connPool get %s\n", c.RemoteAddr(), r.URL.HostPort)
		}
		return sv, nil
	}
	if debug {
		debug.Printf("cli(%s) connPool no conn %s", c.RemoteAddr(), r.URL.HostPort)
	}
	return c.createServerConn(r, direct)
}

func connectDirect2(url *URL, recursive bool) (net.Conn, error) {
	var c net.Conn
	var err error
	c, err = net.Dial("tcp", url.HostPort)
	if err != nil {
		debug.Printf("error direct connect to: %s %v\n", url.HostPort, err)
		if isErrTooManyOpenFd(err) && !recursive {
			return connectDirect2(url, true)
		}
		return nil, err
	}
	// debug.Println("directly connected to", url.HostPort)
	return directConn{c}, nil
}

func connectDirect(url *URL) (net.Conn, error) {
	return connectDirect2(url, false)
}

func isErrTimeout(err error) bool {
	if ne, ok := err.(net.Error); ok {
		return ne.Timeout()
	}
	return false
}

func isHttpErrCode(err error) bool {
	if config.HttpErrorCode <= 0 {
		return false
	}
	if err == CustomHttpErr {
		return true
	}
	return false
}

// MEOW !!!
// Connect to requested server according to whether it's visit count.
// If direct connection fails, try parent proxies.
func (c *clientConn) connect(r *Request, direct bool) (srvconn net.Conn, err error) {
	var errMsg string

	if direct {
		dbgPrintRq(c, r, true)
		if srvconn, err = connectDirect(r.URL); err == nil {
			return
		}
		errMsg = genErrMsg(r, nil, "Direct connection failed.")
		goto fail
	}

	if parentProxy.empty() {
		errMsg = genErrMsg(r, nil, "No parent proxy.")
		goto fail
	}

	// “我向来是不惮以最坏的恶意来揣测中国人的”
	dbgPrintRq(c, r, false)
	if srvconn, err = parentProxy.connect(r.URL); err == nil {
		return
	}
	errMsg = genErrMsg(r, nil, "Parent proxy connection failed.")

fail:
	sendErrorPage(c, "504 Connection failed", err.Error(), errMsg)
	return nil, errPageSent
}

func (c *clientConn) createServerConn(r *Request, direct bool) (*serverConn, error) {
	srvconn, err := c.connect(r, direct)
	if err != nil {
		return nil, err
	}
	sv := newServerConn(srvconn, r.URL.HostPort, direct)
	if debug {
		debug.Printf("cli(%s) connected to %s %d concurrent connections\n",
			c.RemoteAddr(), sv.hostPort, incSrvConnCnt(sv.hostPort))
	}
	return sv, nil
}

// Should call initBuf before reading http response from server. This allows
// us not init buf for connect method which does not need to parse http
// respnose.
func newServerConn(c net.Conn, hostPort string, direct bool) *serverConn {
	sv := &serverConn{
		Conn:     c,
		hostPort: hostPort,
		direct:   direct,
	}
	return sv
}

func (sv *serverConn) isDirect() bool {
	_, ok := sv.Conn.(directConn)
	return ok
}

func (sv *serverConn) initBuf() {
	if sv.bufRd == nil {
		sv.buf = httpBuf.Get()
		sv.bufRd = bufio.NewReaderFromBuf(sv, sv.buf)
	}
}

func (sv *serverConn) releaseBuf() {
	if sv.bufRd != nil {
		// debug.Println("release server buffer")
		httpBuf.Put(sv.buf)
		sv.buf = nil
		sv.bufRd = nil
	}
}

func (sv *serverConn) Close() error {
	sv.releaseBuf()
	if debug {
		debug.Printf("close connection to %s remains %d concurrent connections\n",
			sv.hostPort, decSrvConnCnt(sv.hostPort))
	}
	return sv.Conn.Close()
}

func (sv *serverConn) mayBeClosed() bool {
	if _, ok := sv.Conn.(meowConn); ok {
		debug.Println("meow parent would keep alive")
		return false
	}
	return time.Now().After(sv.willCloseOn)
}

// Use smaller buffer for connection method as the buffer will be hold for a
// very long time.
const connectBufSize = 4096

// Hold at most 2M memory for connection buffer. This can support 256
// concurrent connect method.
var connectBuf = leakybuf.NewLeakyBuf(512, connectBufSize)

func copyServer2Client(sv *serverConn, c *clientConn, r *Request) (err error) {
	buf := connectBuf.Get()
	defer func() {
		connectBuf.Put(buf)
	}()

	/*
		// force retry for debugging
		if r.tryCnt == 1 && sv.maybeFake() {
			time.Sleep(1)
			return RetryError{errors.New("debug retry in copyServer2Client")}
		}
	*/

	total := 0
	for {
		// debug.Println("srv->cli")
		var n int
		if n, err = sv.Read(buf); err != nil {
			// Expected error besides EOF: "use of closed network connection",
			// this is to make blocking read return.
			// debug.Printf("copyServer2Client read data: %v\n", err)
			return
		}
		total += n
		if _, err = c.Write(buf[0:n]); err != nil {
			// debug.Printf("copyServer2Client write data: %v\n", err)
			return
		}
		// debug.Printf("srv(%s)->cli(%s) sent %d bytes data\n", r.URL.HostPort, c.RemoteAddr(), total)
		// set state to rsRecvBody to indicate the request has partial response sent to client
		r.state = rsRecvBody
		sv.state = svSendRecvResponse
	}
}

type serverWriter struct {
	rq *Request
	sv *serverConn
}

func newServerWriter(r *Request, sv *serverConn) *serverWriter {
	return &serverWriter{r, sv}
}

// Write to server, store written data in request buffer if necessary.
// We have to save request body in order to retry request.
// FIXME: too tighly coupled with Request.
func (sw *serverWriter) Write(p []byte) (int, error) {
	if sw.rq.raw == nil {
		// buffer released
	} else if sw.rq.raw.Len() >= 2*httpBufSize {
		// Avoid using too much memory to hold request body. If a request is
		// not buffered completely, meow can't retry and can release memory
		// immediately.
		debug.Println(sw.rq, "request body too large, not buffering any more")
		sw.rq.releaseBuf()
		sw.rq.partial = true
	} else if sw.rq.responseNotSent() {
		sw.rq.raw.Write(p)
	} else { // has sent response, happens when saving data for CONNECT method
		sw.rq.releaseBuf()
	}
	return sw.sv.Write(p)
}

func copyClient2Server(c *clientConn, sv *serverConn, r *Request, srvStopped notification, done chan struct{}) (err error) {
	var n int

	w := newServerWriter(r, sv)
	if c.bufRd != nil {
		n = c.bufRd.Buffered()
		if n > 0 {
			buffered, _ := c.bufRd.Peek(n) // should not return error
			if _, err = w.Write(buffered); err != nil {
				// debug.Printf("cli->srv write buffered err: %v\n", err)
				return
			}
		}
		if debug {
			debug.Printf("cli(%s)->srv(%s) released read buffer\n",
				c.RemoteAddr(), r.URL.HostPort)
		}
		c.releaseBuf()
	}

	buf := connectBuf.Get()
	defer func() {
		connectBuf.Put(buf)
	}()
	for {
		// debug.Println("908: cli->srv")
		if n, err = c.Read(buf); err != nil {
			if isErrTimeout(err) && !srvStopped.hasNotified() {
				debug.Printf("911: cli(%s)->srv(%s) timeout\n", c.RemoteAddr(), r.URL.HostPort)
				continue
			}
			debug.Printf("914: cli->srv read err: %v\n", err)
			return
		}

		// copyServer2Client will detect write to closed server. Just store client content for retry.
		if _, err = w.Write(buf[:n]); err != nil {
			// XXX is it enough to only do block detection in copyServer2Client?
			debug.Printf("921: cli->srv write err: %v\n", err)
			return
		}
		// debug.Printf("924: cli(%s)->srv(%s) sent %d bytes data\n", c.RemoteAddr(), r.URL.HostPort, n)
	}
}

var connEstablished = []byte("HTTP/1.1 200 Tunnel established\r\n\r\n")

// Do HTTP CONNECT
func (sv *serverConn) doConnect(r *Request, c *clientConn) (err error) {
	r.state = rsCreated

	_, isHttpConn := sv.Conn.(httpConn)
	_, isMeowConn := sv.Conn.(meowConn)
	if isHttpConn || isMeowConn {
		if debug {
			debug.Printf("cli(%s) send CONNECT request to parent\n", c.RemoteAddr())
		}
		if err = sv.sendHTTPProxyRequestHeader(r, c); err != nil {
			debug.Printf("cli(%s) error send CONNECT request to parent: %v\n",
				c.RemoteAddr(), err)
			return err
		}
	} else {
		// debug.Printf("send connection confirmation to %s->%s\n", c.RemoteAddr(), r.URL.HostPort)
		if _, err = c.Write(connEstablished); err != nil {
			debug.Printf("cli(%s) error send 200 Connecion established: %v\n",
				c.RemoteAddr(), err)
			return err
		}
	}

	var cli2srvErr error
	done := make(chan struct{})
	srvStopped := newNotification()
	go func() {
		debug.Printf("989: doConnect: cli(%s)->srv(%s)\n", c.RemoteAddr(), r.URL.HostPort)
		cli2srvErr = copyClient2Server(c, sv, r, srvStopped, done)
		// Close sv to force read from server in copyServer2Client return.
		// Note: there's no other code closing the server connection for CONNECT.
		sv.Close()
	}()

	// debug.Printf("doConnect: srv(%s)->cli(%s)\n", r.URL.HostPort, c.RemoteAddr())
	err = copyServer2Client(sv, c, r)
	if isErrTimeout(err) || isErrConnReset(err) || isHttpErrCode(err) {
		srvStopped.notify()
		<-done
	} else {
		// close client connection to force read from client in copyClient2Server return
		c.Conn.Close()
	}
	if cli2srvErr != nil {
		return cli2srvErr
	}
	return
}

func (sv *serverConn) sendHTTPProxyRequestHeader(r *Request, c *clientConn) (err error) {
	if _, err = sv.Write(r.proxyRequestLine()); err != nil {
		return c.handleServerWriteError(r, sv, err,
			"send proxy request line to http parent")
	}
	if hc, ok := sv.Conn.(httpConn); ok && hc.parent.authHeader != nil {
		// Add authorization header for parent http proxy
		if _, err = sv.Write(hc.parent.authHeader); err != nil {
			return c.handleServerWriteError(r, sv, err,
				"send proxy authorization header to http parent")
		}
	}
	// When retry, body is in raw buffer.
	if _, err = sv.Write(r.rawHeaderBody()); err != nil {
		return c.handleServerWriteError(r, sv, err,
			"send proxy request header to http parent")
	}
	/*
		if bool(dbgRq) && verbose {
			debug.Printf("request to http proxy:\n%s%s", r.proxyRequestLine(), r.rawHeaderBody())
		}
	*/
	return
}

func (sv *serverConn) sendRequestHeader(r *Request, c *clientConn) (err error) {
	// Send request to the server
	switch sv.Conn.(type) {
	case httpConn, meowConn:
		return sv.sendHTTPProxyRequestHeader(r, c)
	}
	/*
		if bool(debug) && verbose {
			debug.Printf("request to server\n%s", r.rawRequest())
		}
	*/
	if _, err = sv.Write(r.rawRequest()); err != nil {
		err = c.handleServerWriteError(r, sv, err, "send request to server")
	}
	return
}

func (sv *serverConn) sendRequestBody(r *Request, c *clientConn) (err error) {
	// Send request body. If this is retry, r.raw contains request body and is
	// sent while sending raw request.
	if !r.hasBody() {
		return
	}

	err = sendBody(newServerWriter(r, sv), c.bufRd, int(r.ContLen), r.Chunking)
	if err != nil {
		errl.Printf("cli(%s) send request body error %v %s\n", c.RemoteAddr(), err, r)
		if isErrOpWrite(err) {
			err = c.handleServerWriteError(r, sv, err, "send request body")
		}
		return
	}
	if debug {
		debug.Printf("cli(%s) request body sent %s\n", c.RemoteAddr(), r)
	}
	return
}

// Do HTTP request other that CONNECT
func (sv *serverConn) doRequest(c *clientConn, r *Request, rp *Response) (err error) {
	r.state = rsCreated
	if err = sv.sendRequestHeader(r, c); err != nil {
		return
	}
	if err = sv.sendRequestBody(r, c); err != nil {
		return
	}
	r.state = rsSent
	return c.readResponse(sv, r, rp)
}

// Send response body if header specifies content length
func sendBodyWithContLen(w io.Writer, r *bufio.Reader, contLen int) (err error) {
	// debug.Println("Sending body with content length", contLen)
	if contLen == 0 {
		return
	}
	if err = copyN(w, r, contLen, httpBufSize); err != nil {
		debug.Println("sendBodyWithContLen error:", err)
	}
	return
}

// Use this function until we find Trailer headers actually in use.
func skipTrailer(r *bufio.Reader) error {
	// It's possible to get trailer headers, but the body will always end with
	// a line with just CRLF.
	for {
		s, err := r.ReadSlice('\n')
		if err != nil {
			errl.Println("skip trailer:", err)
			return err
		}
		if len(s) == 2 && s[0] == '\r' && s[1] == '\n' {
			return nil
		}
		errl.Printf("skip trailer: %#v", string(s))
		if len(s) == 1 || len(s) == 2 {
			return fmt.Errorf("malformed chunk body end: %#v", string(s))
		}
	}
}

func skipCRLF(r *bufio.Reader) (err error) {
	var buf [2]byte
	if _, err = io.ReadFull(r, buf[:]); err != nil {
		errl.Println("skip chunk body end:", err)
		return
	}
	if buf[0] != '\r' || buf[1] != '\n' {
		return fmt.Errorf("malformed chunk body end: %#v", string(buf[:]))
	}
	return
}

// Send response body if header specifies chunked encoding. rdSize specifies
// the size of each read on Reader, it should be set to be the buffer size of
// the Reader, this parameter is added for testing.
func sendBodyChunked(w io.Writer, r *bufio.Reader, rdSize int) (err error) {
	// debug.Println("Sending chunked body")
	for {
		var s []byte
		// Read chunk size line, ignore chunk extension if any.
		if s, err = r.PeekSlice('\n'); err != nil {
			errl.Println("peek chunk size:", err)
			return
		}
		smid := bytes.IndexByte(s, ';')
		if smid == -1 {
			smid = len(s)
		} else {
			// use error log to find usage of chunk extension
			errl.Printf("got chunk extension: %s\n", s)
		}
		var size int64
		if size, err = ParseIntFromBytes(TrimSpace(s[:smid]), 16); err != nil {
			errl.Println("chunk size invalid:", err)
			return
		}
		/*
			if debug {
				// To debug getting malformed response status line with "0\r\n".
				if c, ok := w.(*clientConn); ok {
					debug.Printf("cli(%s) chunk size %d %#v\n", c.RemoteAddr(), size, string(s))
				}
			}
		*/
		if size == 0 {
			r.Skip(len(s))
			if err = skipCRLF(r); err != nil {
				return
			}
			if _, err = w.Write([]byte(chunkEnd)); err != nil {
				debug.Println("send chunk ending:", err)
			}
			return
		}
		// RFC 2616 19.3 only suggest tolerating single LF for
		// headers, not for chunked encoding. So assume the server will send
		// CRLF. If not, the following parse int may find errors.
		total := len(s) + int(size) + 2 // total data size for this chunk, including ending CRLF
		// PeekSlice will not advance reader, so we can just copy total sized data.
		if err = copyN(w, r, total, rdSize); err != nil {
			debug.Println("copy chunked data:", err)
			return
		}
	}
}

const chunkEnd = "0\r\n\r\n"

func sendBodySplitIntoChunk(w io.Writer, r *bufio.Reader) (err error) {
	// debug.Printf("sendBodySplitIntoChunk called\n")
	var b []byte
	for {
		b, err = r.ReadNext()
		// debug.Println("split into chunk n =", n, "err =", err)
		if err != nil {
			if err == io.EOF {
				// EOF is expected here as the server is closing connection.
				// debug.Println("end chunked encoding")
				_, err = w.Write([]byte(chunkEnd))
				if err != nil {
					debug.Println("write chunk end 0", err)
				}
				return
			}
			debug.Println("read error in sendBodySplitIntoChunk", err)
			return
		}

		chunkSize := []byte(fmt.Sprintf("%x\r\n", len(b)))
		if _, err = w.Write(chunkSize); err != nil {
			debug.Printf("write chunk size %v\n", err)
			return
		}
		if _, err = w.Write(b); err != nil {
			debug.Println("write chunk data:", err)
			return
		}
		if _, err = w.Write([]byte(CRLF)); err != nil {
			debug.Println("write chunk ending CRLF:", err)
			return
		}
	}
}

// Send message body.
func sendBody(w io.Writer, bufRd *bufio.Reader, contLen int, chunk bool) (err error) {
	// chunked encoding has precedence over content length
	// meow does not sanitize response header, but can correctly handle it
	if chunk {
		err = sendBodyChunked(w, bufRd, httpBufSize)
	} else if contLen >= 0 {
		// It's possible to have content length 0 if server response has no
		// body.
		err = sendBodyWithContLen(w, bufRd, int(contLen))
	} else {
		// Must be reading server response here, because sendBody is called in
		// reading response iff chunked or content length > 0.
		err = sendBodySplitIntoChunk(w, bufRd)
	}
	return
}
