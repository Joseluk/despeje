package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHUpstream abre conexiones TCP desde el VPS usando canales direct-tcpip
// (lo mismo que `ssh -W`). En el VPS solo hace falta sshd.
type SSHUpstream struct {
	addr      string
	config    *ssh.ClientConfig
	keepalive time.Duration

	mu        sync.Mutex
	client    *ssh.Client
	connected atomic.Bool
}

func NewSSHUpstream(c VPSConfig) (*SSHUpstream, error) {
	signer, err := loadSigner(c.Key)
	if err != nil {
		return nil, err
	}
	hostKeyCB, err := knownhosts.New(c.KnownHosts)
	if err != nil {
		return nil, fmt.Errorf("known_hosts %s: %w (conéctate antes una vez con `ssh %s@%s`)", c.KnownHosts, err, c.User, c.Host)
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	algos, err := hostKeyAlgorithms(hostKeyCB, addr)
	if err != nil {
		return nil, err
	}
	return &SSHUpstream{
		addr:      addr,
		keepalive: 20 * time.Second,
		config: &ssh.ClientConfig{
			User:              c.User,
			Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback:   hostKeyCB,
			HostKeyAlgorithms: algos,
			Timeout:           15 * time.Second,
		},
	}, nil
}

func loadSigner(path string) (ssh.Signer, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("clave SSH: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		pass := os.Getenv("DESPEJE_KEY_PASSPHRASE")
		if pass == "" {
			return nil, fmt.Errorf("la clave %s tiene passphrase: usa una clave dedicada sin ella o define DESPEJE_KEY_PASSPHRASE", path)
		}
		return ssh.ParsePrivateKeyWithPassphrase(pem, []byte(pass))
	}
	if err != nil {
		return nil, fmt.Errorf("clave SSH %s: %w", path, err)
	}
	return signer, nil
}

// hostKeyAlgorithms limita la negociación a los tipos de clave que hay en
// known_hosts para ese host; si no, x/crypto/ssh puede elegir otro tipo y
// fallar con "key mismatch" aunque la clave correcta esté guardada.
func hostKeyAlgorithms(cb ssh.HostKeyCallback, addr string) ([]string, error) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	probe, _ := ssh.NewPublicKey(pub)
	err := cb(addr, &net.TCPAddr{IP: net.IPv4zero}, probe)
	var ke *knownhosts.KeyError
	if !errors.As(err, &ke) || len(ke.Want) == 0 {
		return nil, fmt.Errorf("%s no está en known_hosts: conéctate una vez con ssh para guardar su huella", addr)
	}
	var algos []string
	seen := map[string]bool{}
	add := func(a ...string) {
		for _, x := range a {
			if !seen[x] {
				seen[x] = true
				algos = append(algos, x)
			}
		}
	}
	for _, k := range ke.Want {
		switch t := k.Key.Type(); t {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			add(t)
		}
	}
	return algos, nil
}

func (u *SSHUpstream) Connected() bool { return u.connected.Load() }

func (u *SSHUpstream) clientFor(ctx context.Context) (*ssh.Client, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.client != nil {
		return u.client, nil
	}

	d := net.Dialer{Timeout: u.config.Timeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", u.addr)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(u.config.Timeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, u.addr, u.config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})

	client := ssh.NewClient(c, chans, reqs)
	u.client = client
	u.connected.Store(true)
	log.Printf("conectado al VPS %s (%s)", u.addr, c.ServerVersion())
	go u.keepaliveLoop(client)
	go func() {
		err := client.Wait()
		if u.drop(client) {
			log.Printf("conexión con el VPS cerrada: %v", err)
		}
	}()
	return client, nil
}

// drop olvida el cliente si sigue siendo el actual; devuelve si lo era.
func (u *SSHUpstream) drop(c *ssh.Client) bool {
	u.mu.Lock()
	current := u.client == c
	if current {
		u.client = nil
		u.connected.Store(false)
	}
	u.mu.Unlock()
	c.Close()
	return current
}

// alive comprueba con un keepalive si la sesión SSH sigue respondiendo.
func alive(c *ssh.Client, timeout time.Duration) bool {
	res := make(chan error, 1)
	go func() {
		_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
		res <- err
	}()
	select {
	case err := <-res:
		return err == nil
	case <-time.After(timeout):
		return false
	}
}

func (u *SSHUpstream) keepaliveLoop(c *ssh.Client) {
	t := time.NewTicker(u.keepalive)
	defer t.Stop()
	for range t.C {
		if !alive(c, 15*time.Second) {
			if u.drop(c) {
				log.Printf("el VPS no responde al keepalive; reconectando")
			}
			return
		}
	}
}

// channelOpenTimeout limita cuánto esperamos a que el VPS abra la conexión.
var channelOpenTimeout = 15 * time.Second

func (u *SSHUpstream) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		c, err := u.clientFor(ctx)
		if err != nil {
			return nil, fmt.Errorf("conectando al VPS: %w", err)
		}
		dctx, cancel := context.WithTimeout(ctx, channelOpenTimeout)
		conn, err := c.DialContext(dctx, network, addr)
		cancel()
		if err == nil {
			return conn, nil
		}
		// OpenChannelError = el VPS rechazó/no alcanzó el destino; la sesión está bien.
		var oce *ssh.OpenChannelError
		if errors.As(err, &oce) || ctx.Err() != nil || attempt > 0 {
			return nil, err
		}
		// Timeout: si la sesión responde, el problema es el destino y no
		// tiramos los demás túneles; si no, está colgada y reconectamos.
		if errors.Is(err, context.DeadlineExceeded) && alive(c, 5*time.Second) {
			return nil, fmt.Errorf("el VPS no consiguió conectar con %s a tiempo", addr)
		}
		u.drop(c)
	}
}

func (u *SSHUpstream) Close() {
	u.mu.Lock()
	c := u.client
	u.mu.Unlock()
	if c != nil {
		u.drop(c)
	}
}
