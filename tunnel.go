package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Tunnel struct {
	resolver      *Resolver
	logger        *slog.Logger
	addr          string
	listener      net.Listener
	authenticator Authenticator

	connectionId atomic.Int64
	connections  map[int64]*tunnelCtx
	mu           sync.Mutex

	running atomic.Bool
	closeCh chan struct{}
}

func NewTunnel(
	cfg *Config,
	logger *slog.Logger,
	resolver *Resolver,
) *Tunnel {
	t := &Tunnel{
		resolver:    resolver,
		logger:      logger,
		addr:        cfg.TunnelAddr,
		connections: make(map[int64]*tunnelCtx),
		closeCh:     make(chan struct{}),
	}

	return t
}

func (p *Tunnel) Start() {
	p.logger.Warn("tunnel start", slog.String("listen_on", p.addr))
	var err error
	p.listener, err = net.Listen("tcp", p.addr)
	if err != nil {
		p.logger.Error("listen failed", slog.String("addr", p.addr),
			slog.Any("err", err))
		return
	}

	p.running.Store(true)
	p.protectRun(p.serve)
}

func (p *Tunnel) protectRun(fn func()) {
	go func() {
		defer func() {
			e := recover()
			if e != nil {
				p.logger.Error("panic", slog.Any("err", e),
					slog.String("stack", string(debug.Stack())))
			}
		}()

		fn()
	}()
}

func (p *Tunnel) serve() {
	for p.running.Load() {
		conn, err := p.listener.Accept()
		if err != nil {
			if !p.running.Load() {
				p.logger.Warn("closed")
			} else {
				p.logger.Error("accept failed", slog.Any("err", err))
				break
			}
		}

		p.protectRun(func() {
			p.HandleConnection(conn)
		})
	}

	p.Stop()
}

func (p *Tunnel) Reload() {
	var connections map[int64]*tunnelCtx
	p.mu.Lock()
	connections = p.connections
	p.connections = map[int64]*tunnelCtx{}
	p.mu.Unlock()

	for _, ctx := range connections {
		p.closeConnection(ctx)
	}
}

func (p *Tunnel) Stop() {
	if !p.running.Load() {
		return
	}
	p.running.Store(false)
	close(p.closeCh)

	if p.listener != nil {
		err := p.listener.Close()
		if err != nil {
			p.logger.Error("close listener failed", slog.Any("err", err))
		}
		p.listener = nil
	}

	var connections map[int64]*tunnelCtx
	p.mu.Lock()
	connections = p.connections
	p.connections = map[int64]*tunnelCtx{}
	p.mu.Unlock()

	for _, ctx := range connections {
		p.closeConnection(ctx)
	}
}

type tunnelCtx struct {
	id int64

	proxyConn     net.Conn
	proxyHostname string
	proxyAddr     string
	remoteConn    net.Conn
	remoteAddr    string

	established bool
	header      textproto.MIMEHeader
	httpVersion string
}

const (
	partCommand = iota
	partHeader
	partEndOfHeader
)

func (p *Tunnel) HandleConnection(proxyConn net.Conn) {
	if proxyConn == nil { // FIXME maybe listener closed???
		return
	}
	ctx := &tunnelCtx{
		id:        p.connectionId.Add(1),
		proxyConn: proxyConn,
	}
	if ra := proxyConn.RemoteAddr(); ra != nil {
		ctx.proxyAddr = ra.String()
	}

	defer func() {
		p.closeConnection(ctx)
	}()

	remainBuf, err := p.handleConnectRequest(ctx)
	if err != nil {
		p.logger.Error("parse connect request failed", slog.Any("err", err))
		if err = p.responseHttp(ctx, http.StatusBadRequest, nil); err != nil {
			p.logger.Error("response failed",
				slog.Int("code", http.StatusBadRequest), slog.Any("err", err))
		}
		return
	}
	if err = p.responseHttp(ctx, http.StatusOK, nil); err != nil {
		p.logger.Error("response failed",
			slog.Int("code", http.StatusOK), slog.Any("err", err))
	}

	if err = p.createRemoteConnection(ctx); err != nil {
		p.logger.Error("connect failed",
			slog.String("remote", ctx.proxyHostname), slog.Any("err", err))
		return
	}

	p.logger.Info("connected", slog.String("from", ctx.proxyAddr),
		slog.String("proxyTo", ctx.proxyHostname),
		slog.String("remote", ctx.remoteAddr),
	)
	ctx.established = true

	p.putEstablishedConnection(ctx)

	errChan := make(chan error, 2)
	go p.pipe(ctx.remoteConn, remainBuf, errChan)
	go p.pipe(ctx.proxyConn, ctx.remoteConn, errChan)
	if err = <-errChan; err != nil {
		if !errors.Is(err, io.EOF) {
			time.Sleep(time.Millisecond * 300)
		}
	}
}

func (p *Tunnel) handleConnectRequest(ctx *tunnelCtx) (buf *bufio.Reader, err error) {
	buf = bufio.NewReader(ctx.proxyConn)
	reader := textproto.NewReader(buf)
	part := partCommand
	for p.running.Load() {
		switch part {
		case partCommand:
			var line string
			line, err = reader.ReadLine()
			line = strings.TrimSpace(line)
			sps := strings.Split(line, " ")
			tmp := sps[:0]
			for _, s := range sps {
				if len(s) > 0 {
					tmp = append(tmp, s)
				}
			}
			sps = tmp
			if len(sps) != 3 {
				if err == nil {
					err = fmt.Errorf("wrong command format")
				} else {
					err = fmt.Errorf("wrong command format[%s]: %w", line, err)
				}
				return
			}
			if !strings.EqualFold("connect", sps[0]) {
				if err == nil {
					err = fmt.Errorf("unsupport method: %s", sps[0])
				} else {
					err = fmt.Errorf("unsupport method: %s, err: %v", sps[0], err)
				}
				return
			}

			ctx.proxyHostname = sps[1]
			ctx.httpVersion = sps[2]

			part = partHeader
		case partHeader:
			ctx.header, err = reader.ReadMIMEHeader()
			if err != nil {
				return
			}
			part = partEndOfHeader
		case partEndOfHeader:
			return
		}
	}
	if part != partEndOfHeader {
		err = fmt.Errorf("truncate request")
	}

	return
}

func (p *Tunnel) responseHttp(ctx *tunnelCtx, code int, header textproto.MIMEHeader) error {
	var message string
	switch code {
	case http.StatusOK:
		message = "Connection Established"
	case http.StatusBadRequest:
		message = "Bad Request"
	case http.StatusUnauthorized:
		message = "Unauthorized"
	default:
		code = http.StatusBadRequest
		message = "Bad Request"
	}
	if len(ctx.httpVersion) == 0 {
		ctx.httpVersion = "HTTP/1.0"
	}

	buf := bytes.NewBuffer(nil)
	buf.WriteString(ctx.httpVersion)
	buf.WriteByte(' ')
	buf.WriteString(strconv.FormatInt(int64(code), 10))
	buf.WriteByte(' ')
	buf.WriteString(message)
	buf.WriteString("\r\n")

	if len(header) > 0 {
		for key, vals := range header {
			buf.WriteString(key)
			buf.WriteString(": ")
			buf.WriteString(strings.Join(vals, "; "))
			buf.WriteString("\r\n")
		}
	}

	buf.WriteString("\r\n")

	_, err := io.Copy(ctx.proxyConn, buf)
	return err
}

func (p *Tunnel) createRemoteConnection(ctx *tunnelCtx) error {
	if !p.running.Load() {
		return fmt.Errorf("tunnel closed")
	}
	hostname, port, _ := strings.Cut(ctx.proxyHostname, ":")
	addr := ctx.proxyHostname
	remoteIp, found := p.resolver.Resolve(hostname)
	if found {
		addr = remoteIp + ":" + port
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	ctx.remoteConn = conn
	ctx.remoteAddr = conn.RemoteAddr().String()
	return nil
}

func (p *Tunnel) closeConnectionLocked(ctx *tunnelCtx) {
	if ctx.proxyConn != nil {
		if err := ctx.proxyConn.Close(); err != nil {
			p.logger.Error("close proxy connection failed", slog.Any("err", err))
		}
	}
	if ctx.remoteConn != nil {
		if err := ctx.remoteConn.Close(); err != nil {
			p.logger.Error("close remote connection failed", slog.Any("err", err))
		}
	}

	ctx.established = false
	delete(p.connections, ctx.id)
}

func (p *Tunnel) closeConnection(ctx *tunnelCtx) {
	p.mu.Lock()
	p.closeConnectionLocked(ctx)
	p.mu.Unlock()
}

func (p *Tunnel) putEstablishedConnection(ctx *tunnelCtx) {
	p.mu.Lock()
	p.connections[ctx.id] = ctx
	p.mu.Unlock()
}

func (p *Tunnel) pipe(dst io.Writer, src io.Reader, errChan chan<- error) {
	buf := make([]byte, 128*1024)
	for {
		if !p.running.Load() {
			errChan <- fmt.Errorf("tunnel closed")
			return
		}
		n, err := io.CopyBuffer(dst, src, buf)
		if err == nil && n == 0 {
			err = io.EOF
		}
		if err != nil {
			errChan <- err
			return
		}
	}
}
