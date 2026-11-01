package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"sort"
	"strings"
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

// Una IP que falló en directo y sí funcionó por el VPS se recuerda learnTTL
// como máximo; cada learnRecheck se vuelve a probar en directo y, si
// responde, se olvida (un fallo puntual no debe mandar un sitio al VPS, donde
// algunos servicios rechazan IPs de centros de datos).
var (
	learnTTL     = 30 * time.Minute
	learnRecheck = 2 * time.Minute
)

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
	ViaVPSDomains []string
	DirectDomains []string
	Fallback      bool
	DirectTimeout time.Duration
	Upstream      ContextDialer // nil = sin VPS
	DirectDialer  ContextDialer // nil = net.Dialer
	Resolver      *net.Resolver
	Verbose       bool

	learnedMu sync.Mutex
	learned   map[netip.Addr]*learnedIP

	nDirect, nVPS, nFallback, nFail atomic.Int64

	recentMu sync.Mutex
	recent   []Route
}

type learnedIP struct {
	exp  time.Time
	host string
	port string
}

// LearnedIP es lo que /status enseña de cada IP aprendida.
type LearnedIP struct {
	IP    string    `json:"ip"`
	Host  string    `json:"host"`
	Hasta time.Time `json:"hasta"`
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
	if matchDomain(host, r.DirectDomains) {
		ips, err := r.resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		conn, _, err := r.dialDirect(ctx, ips, port)
		if err == nil {
			r.nDirect.Add(1)
		}
		return conn, err
	}
	if matchDomain(host, r.ViaVPSDomains) {
		ips, _ := r.resolve(ctx, host)
		return r.dialVPS(ctx, addr, ips, "manual")
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
		if c, _, derr := r.dialDirect(ctx, ips, port); derr == nil {
			r.nDirect.Add(1)
			return c, nil
		}
		return nil, err
	}

	conn, timedOut, err := r.dialDirect(ctx, ips, port)
	if err == nil {
		r.nDirect.Add(1)
		if r.Verbose {
			log.Printf("directo  %s", addr)
		}
		return conn, nil
	}
	// El bloqueo de LaLiga descarta paquetes en silencio: solo un timeout lo
	// parece. Un reset o "connection refused" es un fallo normal del sitio.
	if !r.Fallback || r.Upstream == nil || ctx.Err() != nil || allLocal(ips) || !timedOut || !r.blocksActive() {
		r.nFail.Add(1)
		return nil, err
	}
	conn, verr := r.dialVPS(ctx, addr, ips, "fallback")
	if verr != nil {
		return nil, err
	}
	r.learn(ips, host, port)
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

// dialDirect devuelve también si algún intento acabó en timeout.
func (r *Router) dialDirect(ctx context.Context, ips []netip.Addr, port string) (_ net.Conn, timedOut bool, _ error) {
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
			return conn, false, nil
		}
		lastErr = err
		timedOut = timedOut || isTimeout(err)
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("sin direcciones")
	}
	return nil, timedOut, lastErr
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// blocksActive dice si hay bloqueos en curso (o si no lo sabemos aún).
func (r *Router) blocksActive() bool {
	if r.Blocked == nil || r.Blocked.Set() == nil {
		return true
	}
	return r.Blocked.Set().Len() > 0
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

func (r *Router) learn(ips []netip.Addr, host, port string) {
	r.learnedMu.Lock()
	defer r.learnedMu.Unlock()
	if r.learned == nil {
		r.learned = make(map[netip.Addr]*learnedIP)
	}
	exp := time.Now().Add(learnTTL)
	for _, ip := range ips {
		r.learned[ip] = &learnedIP{exp: exp, host: host, port: port}
	}
}

func (r *Router) isLearned(ip netip.Addr) bool {
	r.learnedMu.Lock()
	defer r.learnedMu.Unlock()
	l, ok := r.learned[ip]
	if ok && time.Now().After(l.exp) {
		delete(r.learned, ip)
		return false
	}
	return ok
}

func (r *Router) forget(ip netip.Addr) {
	r.learnedMu.Lock()
	delete(r.learned, ip)
	r.learnedMu.Unlock()
}

func (r *Router) learnedList() []LearnedIP {
	r.learnedMu.Lock()
	defer r.learnedMu.Unlock()
	out := make([]LearnedIP, 0, len(r.learned))
	for ip, l := range r.learned {
		out = append(out, LearnedIP{IP: ip.String(), Host: l.host, Hasta: l.exp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hasta.Before(out[j].Hasta) })
	return out
}

// RecheckLearned vuelve a probar en directo las IPs aprendidas y olvida las
// que responden.
func (r *Router) RecheckLearned(ctx context.Context) {
	t := time.NewTicker(learnRecheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.recheckOnce(ctx)
		}
	}
}

func (r *Router) recheckOnce(ctx context.Context) {
	type probe struct {
		ip   netip.Addr
		host string
		port string
	}
	var probes []probe
	r.learnedMu.Lock()
	for ip, l := range r.learned {
		if time.Now().After(l.exp) {
			delete(r.learned, ip)
			continue
		}
		probes = append(probes, probe{ip, l.host, l.port})
	}
	r.learnedMu.Unlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, p := range probes {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			conn, _, err := r.dialDirect(ctx, []netip.Addr{p.ip}, p.port)
			if err != nil {
				return
			}
			conn.Close()
			r.forget(p.ip)
			log.Printf("directo  %s (%s) vuelve a responder; deja de ir por el VPS", p.ip, p.host)
		}()
	}
	wg.Wait()
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

// matchDomain dice si host es alguno de los dominios o un subdominio suyo.
func matchDomain(host string, domains []string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}
