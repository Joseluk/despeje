package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

type Mode string

const (
	ModeAuto Mode = "auto" // solo IPs bloqueadas ahora (+ fallback)
	ModeCDN  Mode = "cdn"  // todo Cloudflare por el VPS, siempre
	ModeAll  Mode = "all"  // todo el tráfico por el VPS
)

// learnTTL es cuánto recordamos una IP que falló en directo y sí funcionó por el VPS.
const learnTTL = time.Hour

type ContextDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// Router decide, conexión a conexión, si salir en directo o por el VPS.
type Router struct {
	Mode          Mode
	Blocked       *Blocklist
	CDN           *IPSet
	ViaVPS        *IPSet
	Direct        *IPSet
	Fallback      bool
	DirectTimeout time.Duration
	Upstream      ContextDialer // nil = sin VPS
	DirectDialer  ContextDialer // nil = net.Dialer
	Resolver      *net.Resolver
	Verbose       bool

	learnedMu sync.Mutex
	learned   map[netip.Addr]time.Time

	nDirect, nVPS, nFallback, nFail atomic.Int64

	recentMu sync.Mutex
	recent   []Route
}

type Route struct {
	Time   time.Time `json:"hora"`
	Host   string    `json:"host"`
	Reason string    `json:"motivo"`
}

func (r *Router) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := r.resolve(ctx, host)
	if err != nil {
		// Si el DNS local falla, que resuelva el VPS.
		if r.Upstream != nil && ctx.Err() == nil {
			return r.dialVPS(ctx, addr, nil, "dns")
		}
		r.nFail.Add(1)
		return nil, err
	}

	if reason := r.reasonVPS(ips); reason != "" {
		conn, err := r.dialVPS(ctx, addr, ips, reason)
		if err == nil || ctx.Err() != nil {
			return conn, err
		}
		// VPS caído: último intento en directo, por si la lista no aplica a tu operador.
		if c, derr := r.dialDirect(ctx, ips, port); derr == nil {
			r.nDirect.Add(1)
			return c, nil
		}
		return nil, err
	}

	conn, err := r.dialDirect(ctx, ips, port)
	if err == nil {
		r.nDirect.Add(1)
		if r.Verbose {
			log.Printf("directo  %s", addr)
		}
		return conn, nil
	}
	if !r.Fallback || r.Upstream == nil || ctx.Err() != nil || allLocal(ips) {
		r.nFail.Add(1)
		return nil, err
	}
	conn, verr := r.dialVPS(ctx, addr, ips, "fallback")
	if verr != nil {
		return nil, err
	}
	r.learn(ips)
	r.nFallback.Add(1)
	return conn, nil
}

func (r *Router) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a.Unmap()}, nil
	}
	res := r.Resolver
	if res == nil {
		res = net.DefaultResolver
	}
	ips, err := res.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for i := range ips {
		ips[i] = ips[i].Unmap()
	}
	if len(ips) > 4 {
		ips = ips[:4]
	}
	return ips, nil
}

// reasonVPS devuelve por qué ir por el VPS, o "" para ir en directo.
func (r *Router) reasonVPS(ips []netip.Addr) string {
	for _, ip := range ips {
		if r.Direct.Contains(ip) {
			return ""
		}
	}
	for _, ip := range ips {
		if r.ViaVPS.Contains(ip) {
			return "manual"
		}
	}
	if allLocal(ips) {
		return ""
	}
	if r.Mode == ModeAll {
		return "all"
	}
	for _, ip := range ips {
		switch {
		case r.Blocked != nil && r.Blocked.Contains(ip):
			return "bloqueada"
		case r.isLearned(ip):
			return "aprendida"
		case r.Mode == ModeCDN && r.CDN.Contains(ip):
			return "cdn"
		}
	}
	return ""
}

func (r *Router) dialDirect(ctx context.Context, ips []netip.Addr, port string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, r.DirectTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	var d ContextDialer = r.DirectDialer
	if d == nil {
		d = &net.Dialer{KeepAlive: 30 * time.Second}
	}
	var lastErr error
	for i, ip := range ips {
		// Reparte el tiempo entre IPs como hace net.Dialer, con un mínimo razonable.
		per := time.Until(deadline) / time.Duration(len(ips)-i)
		if min := 1500 * time.Millisecond; per < min {
			per = min
		}
		dctx, dcancel := context.WithTimeout(ctx, per)
		conn, err := d.DialContext(dctx, "tcp", net.JoinHostPort(ip.String(), port))
		dcancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("sin direcciones")
	}
	return nil, lastErr
}

// vpsAttemptTimeout es lo que esperamos por cada IP antes de probar la siguiente.
var vpsAttemptTimeout = 5 * time.Second

// dialVPS abre la conexión desde el VPS. Le pasamos IPs ya resueltas aquí
// (IPv4 primero) en vez del nombre: sshd resuelve cada nombre sin caché, lo
// que añade ~0,4 s por conexión y depende de que el DNS del VPS no se atasque.
// Si ninguna IP funciona, probamos con el nombre por si el VPS ve otras.
func (r *Router) dialVPS(ctx context.Context, addr string, ips []netip.Addr, reason string) (net.Conn, error) {
	if r.Upstream == nil {
		return nil, errors.New("no hay VPS configurado")
	}
	conn, err := r.dialVPSTargets(ctx, addr, ips)
	if err != nil {
		r.nFail.Add(1)
		log.Printf("VPS      %s (%s): error: %v", addr, reason, err)
		return nil, err
	}
	r.nVPS.Add(1)
	log.Printf("VPS      %s (%s)", addr, reason)
	r.recentMu.Lock()
	r.recent = append(r.recent, Route{Time: time.Now(), Host: addr, Reason: reason})
	if len(r.recent) > 50 {
		r.recent = r.recent[len(r.recent)-50:]
	}
	r.recentMu.Unlock()
	return conn, nil
}

func (r *Router) dialVPSTargets(ctx context.Context, addr string, ips []netip.Addr) (net.Conn, error) {
	host, port, _ := net.SplitHostPort(addr)
	var targets []string
	for _, v4 := range []bool{true, false} {
		for _, ip := range ips {
			if ip.Is4() == v4 {
				targets = append(targets, net.JoinHostPort(ip.String(), port))
			}
		}
	}
	if _, err := netip.ParseAddr(host); err != nil {
		targets = append(targets, addr)
	}

	var lastErr error
	for i, t := range targets {
		actx, cancel := ctx, context.CancelFunc(func() {})
		if i < len(targets)-1 {
			actx, cancel = context.WithTimeout(ctx, vpsAttemptTimeout)
		}
		conn, err := r.Upstream.DialContext(actx, "tcp", t)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func (r *Router) learn(ips []netip.Addr) {
	r.learnedMu.Lock()
	defer r.learnedMu.Unlock()
	if r.learned == nil {
		r.learned = make(map[netip.Addr]time.Time)
	}
	exp := time.Now().Add(learnTTL)
	for _, ip := range ips {
		r.learned[ip] = exp
	}
}

func (r *Router) isLearned(ip netip.Addr) bool {
	r.learnedMu.Lock()
	defer r.learnedMu.Unlock()
	exp, ok := r.learned[ip]
	if ok && time.Now().After(exp) {
		delete(r.learned, ip)
		return false
	}
	return ok
}

func (r *Router) learnedCount() int {
	r.learnedMu.Lock()
	defer r.learnedMu.Unlock()
	return len(r.learned)
}

func (r *Router) recentRoutes() []Route {
	r.recentMu.Lock()
	defer r.recentMu.Unlock()
	return append([]Route(nil), r.recent...)
}

func allLocal(ips []netip.Addr) bool {
	for _, ip := range ips {
		if !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
			return false
		}
	}
	return len(ips) > 0
}
