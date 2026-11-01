package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestIPSet(t *testing.T) {
	s, err := ParseIPList(strings.NewReader("# comentario\n104.18.50.34\n\n2a06:98c1:3120::3\n188.114.96.0/20\nbasura\n"))
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{
		"104.18.50.34":        true,
		"::ffff:104.18.50.34": true,
		"104.18.50.35":        false,
		"188.114.97.5":        true,
		"2a06:98c1:3120::3":   true,
		"2a06:98c1:3120::4":   false,
	} {
		if got := s.Contains(netip.MustParseAddr(ip)); got != want {
			t.Errorf("Contains(%s) = %v, quería %v", ip, got, want)
		}
	}
}

// fakeVPS es un sshd mínimo que solo atiende canales direct-tcpip.
type fakeVPS struct {
	addr       string
	knownHosts string
	keyFile    string
	forwards   atomic.Int64
	redirect   map[string]string // IP pública simulada -> servidor de pruebas
}

func startFakeVPS(t *testing.T) *fakeVPS {
	t.Helper()
	dir := t.TempDir()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	clientPub, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSSHPub, _ := ssh.NewPublicKey(clientPub)

	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	v := &fakeVPS{redirect: map[string]string{}, keyFile: filepath.Join(dir, "id_ed25519"), knownHosts: filepath.Join(dir, "known_hosts")}
	os.WriteFile(v.keyFile, pem.EncodeToMemory(block), 0o600)

	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) == string(clientSSHPub.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("clave desconocida")
	}}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	v.addr = ln.Addr().String()
	line := knownhosts.Line([]string{knownhosts.Normalize(v.addr)}, hostSigner.PublicKey())
	os.WriteFile(v.knownHosts, []byte(line+"\n"), 0o600)

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "direct-tcpip" {
						nc.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					var p struct {
						Host     string
						Port     uint32
						OrigHost string
						OrigPort uint32
					}
					ssh.Unmarshal(nc.ExtraData(), &p)
					if p.Host == "agujero-negro.test" {
						continue // nunca responde: simula un destino que no contesta
					}
					target := net.JoinHostPort(p.Host, fmt.Sprint(p.Port))
					if to, ok := v.redirect[target]; ok {
						target = to
					}
					dst, err := net.Dial("tcp", target)
					if err != nil {
						nc.Reject(ssh.ConnectionFailed, err.Error())
						continue
					}
					ch, creqs, _ := nc.Accept()
					go ssh.DiscardRequests(creqs)
					v.forwards.Add(1)
					go func() { io.Copy(ch, dst); ch.CloseWrite() }()
					go func() { io.Copy(dst, ch); dst.Close() }()
				}
			}()
		}
	}()
	return v
}

type failingDialer struct{ calls atomic.Int64 }

func (f *failingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	f.calls.Add(1)
	return nil, errors.New("simulado: IP bloqueada")
}

func setup(t *testing.T, mutate func(*Router)) (*Router, *fakeVPS, string) {
	t.Helper()
	vps := startFakeVPS(t)
	host, port, _ := net.SplitHostPort(vps.addr)
	var p int
	fmt.Sscan(port, &p)
	up, err := NewSSHUpstream(VPSConfig{Host: host, Port: p, User: "test", Key: vps.keyFile, KnownHosts: vps.knownHosts})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(up.Close)

	empty, _ := NewIPSet(nil)
	r := &Router{Mode: ModeAuto, Blocked: &Blocklist{}, ViaVPS: empty, Direct: empty, Fallback: true,
		DirectTimeout: 2 * time.Second, Upstream: up}
	if mutate != nil {
		mutate(r)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	px := &Proxy{Router: r, Blocked: r.Blocked, Upstream: up, PACAddr: ln.Addr().String(), Mode: r.Mode}
	go px.Serve(ln)
	return r, vps, ln.Addr().String()
}

func clientVia(t *testing.T, ts *httptest.Server, proxyURL string) *http.Client {
	tr := ts.Client().Transport.(*http.Transport).Clone()
	u, _ := url.Parse(proxyURL)
	tr.Proxy = http.ProxyURL(u)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func get(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func newTarget(t *testing.T, tls bool) *httptest.Server {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "gol") })
	var ts *httptest.Server
	if tls {
		ts = httptest.NewTLSServer(h)
	} else {
		ts = httptest.NewServer(h)
	}
	t.Cleanup(ts.Close)
	return ts
}

func TestConnectViaVPS(t *testing.T) {
	ts := newTarget(t, true)
	via, _ := NewIPSet([]string{"127.0.0.1"})
	_, vps, addr := setup(t, func(r *Router) { r.ViaVPS = via })
	if got := get(t, clientVia(t, ts, "http://"+addr), ts.URL); got != "gol" {
		t.Fatalf("respuesta %q", got)
	}
	if vps.forwards.Load() != 1 {
		t.Fatalf("esperaba 1 conexión por el VPS, hubo %d", vps.forwards.Load())
	}
}

func TestSOCKS5Direct(t *testing.T) {
	ts := newTarget(t, false)
	_, vps, addr := setup(t, nil)
	if got := get(t, clientVia(t, ts, "socks5://"+addr), ts.URL); got != "gol" {
		t.Fatalf("respuesta %q", got)
	}
	if vps.forwards.Load() != 0 {
		t.Fatalf("no debía pasar por el VPS")
	}
}

func TestPlainHTTPViaVPS(t *testing.T) {
	ts := newTarget(t, false)
	via, _ := NewIPSet([]string{"127.0.0.0/8"})
	_, vps, addr := setup(t, func(r *Router) { r.ViaVPS = via })
	if got := get(t, clientVia(t, ts, "http://"+addr), ts.URL+"/x"); got != "gol" {
		t.Fatalf("respuesta %q", got)
	}
	if vps.forwards.Load() != 1 {
		t.Fatalf("esperaba 1 conexión por el VPS, hubo %d", vps.forwards.Load())
	}
}

func TestFallbackLearns(t *testing.T) {
	ts := newTarget(t, false)
	fd := &failingDialer{}
	r, vps, _ := setup(t, func(r *Router) { r.DirectDialer = fd })
	blocked := "104.18.50.34:80"
	vps.redirect[blocked] = ts.Listener.Addr().String()

	// 1ª conexión: falla en directo -> fallback por el VPS -> se aprende la IP.
	conn, err := r.DialContext(context.Background(), "tcp", blocked)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if fd.calls.Load() == 0 || r.nFallback.Load() != 1 || vps.forwards.Load() != 1 {
		t.Fatalf("directo=%d fallback=%d vps=%d", fd.calls.Load(), r.nFallback.Load(), vps.forwards.Load())
	}

	// 2ª: ya aprendida, va directa al VPS sin intentar en directo.
	before := fd.calls.Load()
	conn, err = r.DialContext(context.Background(), "tcp", blocked)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if fd.calls.Load() != before || vps.forwards.Load() != 2 {
		t.Fatalf("no reutilizó la IP aprendida: directo=%d vps=%d", fd.calls.Load(), vps.forwards.Load())
	}

	// Una IP local que falla nunca se manda al VPS.
	if _, err := r.DialContext(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("una IP local que falla en directo no debe ir por el VPS")
	}
	if vps.forwards.Load() != 2 {
		t.Fatal("la IP local pasó por el VPS")
	}
}

func TestBlocklistRouting(t *testing.T) {
	b := &Blocklist{}
	s, _ := ParseIPList(strings.NewReader("104.18.50.34\n"))
	b.set.Store(s)
	cdn, _ := NewIPSet(cloudflareRanges)
	r := &Router{Mode: ModeAuto, Blocked: b, CDN: cdn}
	ip := func(s string) []netip.Addr { return []netip.Addr{netip.MustParseAddr(s)} }

	if got := r.reasonVPS(ip("104.18.50.34")); got != "bloqueada" {
		t.Errorf("bloqueada: %q", got)
	}
	if got := r.reasonVPS(ip("104.18.50.35")); got != "" {
		t.Errorf("Cloudflare no bloqueada en modo auto: %q", got)
	}
	r.Mode = ModeCDN
	if got := r.reasonVPS(ip("104.18.50.35")); got != "cdn" {
		t.Errorf("modo cdn: %q", got)
	}
	r.Mode = ModeAll
	if got := r.reasonVPS(ip("192.168.1.1")); got != "" {
		t.Errorf("red local en modo all: %q", got)
	}
	if got := r.reasonVPS(ip("8.8.8.8")); got != "all" {
		t.Errorf("modo all: %q", got)
	}
}

func TestBlocklistFeedETag(t *testing.T) {
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, "1.2.3.4\n5.6.7.8\n")
	}))
	defer ts.Close()
	b := &Blocklist{URL: ts.URL, Client: ts.Client()}
	if changed, err := b.Refresh(context.Background()); err != nil || !changed || b.Set().Len() != 2 {
		t.Fatalf("primera descarga: changed=%v err=%v len=%d", changed, err, b.Set().Len())
	}
	if changed, err := b.Refresh(context.Background()); err != nil || changed || b.Set().Len() != 2 {
		t.Fatalf("304: changed=%v err=%v len=%d", changed, err, b.Set().Len())
	}
}

func TestVPSChannelTimeoutKeepsSession(t *testing.T) {
	old := channelOpenTimeout
	channelOpenTimeout = 300 * time.Millisecond
	defer func() { channelOpenTimeout = old }()

	ts := newTarget(t, false)
	via, _ := NewIPSet([]string{"127.0.0.1"})
	r, vps, _ := setup(t, func(r *Router) { r.ViaVPS = via })
	up := r.Upstream.(*SSHUpstream)

	conn, err := up.DialContext(context.Background(), "tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	before, _ := up.clientFor(context.Background())

	start := time.Now()
	if _, err := up.DialContext(context.Background(), "tcp", "agujero-negro.test:443"); err == nil {
		t.Fatal("esperaba error por timeout")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("tardó demasiado: %v", d)
	}
	after, _ := up.clientFor(context.Background())
	if before != after {
		t.Fatal("un destino que no responde no debe tirar la sesión SSH")
	}
	if vps.forwards.Load() != 1 {
		t.Fatalf("forwards=%d", vps.forwards.Load())
	}
}
