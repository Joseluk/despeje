// despeje: proxy local que saca por tu VPS (vía SSH) solo el tráfico hacia
// las IPs que LaLiga hace bloquear durante los partidos, según
// https://hayahora.futbol/#sobre-los-bloqueos.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// version la fija el build de release con -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfgPath := flag.String("config", defaultConfigPath(), "fichero de configuración")
	check := flag.Bool("check", false, "comprueba la lista de bloqueos y el VPS, y sale")
	sysOff := flag.Bool("sysproxy-off", false, "quita el proxy de despeje de Windows y sale")
	verbose := flag.Bool("v", false, "registra también las conexiones directas")
	keygen := flag.String("keygen", "", "crea (si no existe) una clave SSH en esta ruta, imprime la pública y sale")
	showVersion := flag.Bool("version", false, "muestra la versión y sale")
	flag.Parse()

	if *showVersion {
		fmt.Println("despeje", version)
		return
	}
	if *keygen != "" {
		path := expandPath(*keygen)
		line, err := ensureKey(path)
		if errors.Is(err, fs.ErrPermission) {
			dir := filepath.Dir(path)
			fatal("%v\nWindows deniega el acceso a %s. Si es tu carpeta, restaura sus permisos con:\n  icacls \"%s\" /reset /T", err, dir, dir)
		}
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(line)
		return
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fatal("configuración: %v", err)
	}
	if cfg.LogFile != "" {
		os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755)
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fatal("log: %v", err)
		}
		defer f.Close()
		// despejew.exe no tiene consola: escribir en Stderr falla y MultiWriter
		// abortaría antes de llegar al fichero.
		log.SetOutput(io.MultiWriter(f, bestEffort{os.Stderr}))
	}

	pacAddr := pacAddress(cfg.Listen)
	pacBase := "http://" + pacAddr + "/proxy.pac"
	if *sysOff {
		removed, err := DisableSystemProxy(pacBase)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(map[bool]string{true: "proxy de despeje eliminado de Windows", false: "Windows no estaba usando despeje"}[removed])
		return
	}

	up, err := NewSSHUpstream(cfg.VPS)
	if err != nil {
		fatal("VPS: %v", err)
	}
	defer up.Close()

	cdn, _ := NewIPSet(cloudflareRanges)
	via, viaDomains, err := ParseRules(cfg.ViaVPS)
	if err != nil {
		fatal("via_vps: %v", err)
	}
	direct, directDomains, err := ParseRules(cfg.Direct)
	if err != nil {
		fatal("direct: %v", err)
	}

	bl := NewBlocklist(cfg.ISP)
	router := &Router{
		Mode:          cfg.Mode,
		Blocked:       bl,
		CDN:           cdn,
		ViaVPS:        via,
		Direct:        direct,
		ViaVPSDomains: viaDomains,
		DirectDomains: directDomains,
		Fallback:      *cfg.Fallback,
		DirectTimeout: time.Duration(cfg.DirectTimeout),
		Upstream:      up,
		Verbose:       *verbose,
	}
	// La propia lista se descarga a través del router: si algún día bloquean
	// hayahora.futbol, el fallback la trae por el VPS.
	bl.Client = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext:         router.DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	}}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *check {
		os.Exit(runCheck(ctx, router, bl, up))
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fatal("escuchando en %s: %v", cfg.Listen, err)
	}
	log.Printf("despeje escuchando en %s (HTTP + SOCKS5), modo %s, operador %s, VPS %s@%s",
		cfg.Listen, cfg.Mode, cfg.ISP, cfg.VPS.User, cfg.VPS.Host)

	go bl.Run(ctx, time.Duration(cfg.Refresh))
	go router.RecheckLearned(ctx)
	go func() {
		if _, err := up.clientFor(ctx); err != nil {
			log.Printf("aún no hay conexión con el VPS (se reintentará al usarlo): %v", err)
		}
	}()

	if *cfg.SystemProxy {
		// El parámetro evita que WinINet sirva un PAC cacheado de otra sesión.
		pacURL := fmt.Sprintf("%s?v=%d", pacBase, time.Now().Unix())
		restore, err := EnableSystemProxy(pacURL, pacBase)
		if err != nil {
			log.Printf("no se pudo configurar el proxy de Windows: %v", err)
		} else {
			log.Printf("Windows usa ahora %s", pacURL)
			defer func() {
				restore()
				log.Printf("proxy de Windows restaurado")
			}()
		}
	}

	p := &Proxy{Router: router, Blocked: bl, Upstream: up, PACAddr: pacAddr, Mode: cfg.Mode, ISP: cfg.ISP}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	if err := p.Serve(ln); err != nil && ctx.Err() == nil {
		log.Printf("proxy: %v", err)
	}
	log.Printf("saliendo")
}

func runCheck(ctx context.Context, r *Router, bl *Blocklist, up *SSHUpstream) int {
	status := 0
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	fmt.Printf("Lista de bloqueos (%s)... ", bl.URL)
	if _, err := bl.Refresh(ctx); err != nil {
		fmt.Println("ERROR:", err)
		status = 1
	} else {
		fmt.Printf("OK, %d IPs bloqueadas ahora mismo\n", bl.Set().Len())
	}

	fmt.Printf("SSH al VPS... ")
	if _, err := up.clientFor(ctx); err != nil {
		fmt.Println("ERROR:", err)
		return 1
	}
	fmt.Println("OK")

	fmt.Printf("Salida por el VPS (www.cloudflare.com:443)... ")
	var verr error
	for attempt := 0; attempt < 3; attempt++ {
		t0 := time.Now()
		ips, _ := r.resolve(ctx, "www.cloudflare.com")
		c, err := r.dialVPSTargets(ctx, "www.cloudflare.com:443", ips)
		if verr = err; err == nil {
			c.Close()
			fmt.Printf("OK (%v)\n", time.Since(t0).Round(time.Millisecond))
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if verr != nil {
		status = 1
		fmt.Println("ERROR:", verr)
		var oce *ssh.OpenChannelError
		if errors.As(verr, &oce) && oce.Reason == ssh.Prohibited {
			fmt.Println("  El VPS no deja abrir túneles a este usuario (AllowTcpForwarding / PermitOpen / DisableForwarding).")
		} else {
			fmt.Println("  El VPS acepta el túnel pero no logra salir a internet: revisa su red o un cortafuegos de salida.")
		}
	}

	if ip, ok := bl.Set().First(); ok {
		fmt.Printf("¿Tu conexión bloquea %s?... ", ip)
		d := net.Dialer{Timeout: 4 * time.Second}
		if c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), "443")); err != nil {
			fmt.Println("sí, no responde en directo (despeje la sacará por el VPS)")
		} else {
			c.Close()
			fmt.Println("no, responde en directo")
		}
	}
	if s, err := DescribeSystemProxy(); err == nil {
		fmt.Println("Proxy de Windows ahora:", s)
	}
	return status
}

// pacAddress es la dirección que anunciamos en el PAC (nunca 0.0.0.0).
func pacAddress(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// bestEffort ignora los errores de escritura.
type bestEffort struct{ w io.Writer }

func (b bestEffort) Write(p []byte) (int, error) {
	b.w.Write(p)
	return len(p), nil
}

func fatal(format string, args ...any) {
	log.Printf(format, args...)
	os.Exit(1)
}
