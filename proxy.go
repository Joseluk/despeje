package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"sync"
	"time"
)

// Proxy escucha HTTP (CONNECT y peticiones absolutas) y SOCKS5 en el mismo
// puerto: el primer byte 0x05 identifica SOCKS5. También sirve /proxy.pac y
// /status para peticiones locales.
type Proxy struct {
	Router   *Router
	Blocked  *Blocklist
	Upstream *SSHUpstream
	PACAddr  string // host:port que anuncia el PAC
	Mode     Mode
	ISP      string

	started time.Time
	fwd     *httputil.ReverseProxy
}

func (p *Proxy) Serve(ln net.Listener) error {
	p.started = time.Now()
	p.fwd = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {},
		Transport: &http.Transport{
			DialContext:         p.Router.DialContext,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
		ErrorLog: log.Default(),
	}

	httpLn := newChanListener(ln.Addr())
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go srv.Serve(httpLn)
	defer srv.Close()

	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			br := bufio.NewReader(c)
			c.SetReadDeadline(time.Now().Add(30 * time.Second))
			first, err := br.Peek(1)
			c.SetReadDeadline(time.Time{})
			if err != nil {
				c.Close()
				return
			}
			bc := &bufferedConn{Conn: c, r: br}
			if first[0] == 0x05 {
				p.serveSOCKS5(bc)
				return
			}
			httpLn.ch <- bc
		}()
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		p.handleConnect(w, r)
	case r.URL.IsAbs():
		p.fwd.ServeHTTP(w, r)
	case r.URL.Path == "/proxy.pac":
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprintf(w, pacTemplate, p.PACAddr)
	case r.URL.Path == "/" || r.URL.Path == "/status":
		p.handleStatus(w)
	default:
		http.NotFound(w, r)
	}
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target, err := p.Router.DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		target.Close()
		http.Error(w, "hijack no soportado", http.StatusInternalServerError)
		return
	}
	client, rw, err := hj.Hijack()
	if err != nil {
		target.Close()
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		client.Close()
		target.Close()
		return
	}
	if n := rw.Reader.Buffered(); n > 0 {
		b, _ := rw.Reader.Peek(n)
		target.Write(b)
	}
	pipe(client, target)
}

func (p *Proxy) handleStatus(w http.ResponseWriter) {
	count, updated, lastErr := p.Blocked.Info()
	errStr := ""
	if lastErr != nil {
		errStr = lastErr.Error()
	}
	st := map[string]any{
		"modo":                 p.Mode,
		"operador":             p.ISP,
		"vps_conectado":        p.Upstream != nil && p.Upstream.Connected(),
		"ips_bloqueadas_ahora": count,
		"lista_actualizada":    updated,
		"lista_error":          errStr,
		"ips_aprendidas":       p.Router.learnedCount(),
		"conexiones": map[string]int64{
			"directas": p.Router.nDirect.Load(),
			"vps":      p.Router.nVPS.Load(),
			"fallback": p.Router.nFallback.Load(),
			"fallidas": p.Router.nFail.Load(),
		},
		"activo_desde":    p.started,
		"ultimas_por_vps": p.Router.recentRoutes(),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(st)
}

// SOCKS5 sin autenticación, solo CONNECT (RFC 1928).
func (p *Proxy) serveSOCKS5(c net.Conn) {
	c.SetDeadline(time.Now().Add(30 * time.Second))
	reply := func(code byte) {
		c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	}

	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		c.Close()
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		c.Close()
		return
	}
	c.Write([]byte{5, 0})

	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[0] != 5 {
		c.Close()
		return
	}
	if req[1] != 1 { // solo CONNECT
		reply(7)
		c.Close()
		return
	}
	var host string
	switch req[3] {
	case 1, 4:
		ip := make([]byte, 4)
		if req[3] == 4 {
			ip = make([]byte, 16)
		}
		if _, err := io.ReadFull(c, ip); err != nil {
			c.Close()
			return
		}
		host = net.IP(ip).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			c.Close()
			return
		}
		name := make([]byte, l[0])
		if _, err := io.ReadFull(c, name); err != nil {
			c.Close()
			return
		}
		host = string(name)
	default:
		reply(8)
		c.Close()
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		c.Close()
		return
	}
	addr := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	target, err := p.Router.DialContext(ctx, "tcp", addr)
	cancel()
	if err != nil {
		reply(5)
		c.Close()
		return
	}
	reply(0)
	c.SetDeadline(time.Time{})
	pipe(c, target)
}

// pipe copia en ambos sentidos respetando el medio cierre.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	a.Close()
	b.Close()
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// chanListener entrega a http.Server las conexiones que no son SOCKS5.
type chanListener struct {
	ch   chan net.Conn
	addr net.Addr
	once sync.Once
	done chan struct{}
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{ch: make(chan net.Conn), addr: addr, done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, errors.New("listener cerrado")
	}
}

func (l *chanListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *chanListener) Addr() net.Addr { return l.addr }

// Si despeje no está corriendo, el navegador cae a DIRECT y sigue funcionando.
const pacTemplate = `function FindProxyForURL(url, host) {
  if (isPlainHostName(host) || host == "localhost" || shExpMatch(host, "*.local") ||
      host == "::1") return "DIRECT";
  if (/^\d+\.\d+\.\d+\.\d+$/.test(host) &&
      (isInNet(host, "127.0.0.0", "255.0.0.0") || isInNet(host, "10.0.0.0", "255.0.0.0") ||
       isInNet(host, "172.16.0.0", "255.240.0.0") || isInNet(host, "192.168.0.0", "255.255.0.0") ||
       isInNet(host, "169.254.0.0", "255.255.0.0"))) return "DIRECT";
  return "PROXY %[1]s; SOCKS5 %[1]s; DIRECT";
}
`
