package main

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
)

// IPSet guarda IPs sueltas (búsqueda O(1)) y rangos CIDR.
type IPSet struct {
	addrs    map[netip.Addr]struct{}
	prefixes []netip.Prefix
}

func NewIPSet(entries []string) (*IPSet, error) {
	s := &IPSet{addrs: make(map[netip.Addr]struct{})}
	for _, e := range entries {
		if err := s.add(e); err != nil {
			return nil, err
		}
	}
	return s, nil
}

var domainRe = regexp.MustCompile(`^([a-z0-9-]+\.)+[a-z0-9-]+$`)

// ParseRules separa las entradas de via_vps/direct en IPs/CIDR y dominios.
// Un dominio ("tiktok.com" o "*.tiktok.com") incluye sus subdominios.
func ParseRules(entries []string) (*IPSet, []string, error) {
	var ips, domains []string
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if _, err := netip.ParseAddr(e); err == nil || strings.Contains(e, "/") {
			ips = append(ips, e)
			continue
		}
		d := strings.Trim(strings.TrimPrefix(e, "*."), ".")
		if !domainRe.MatchString(d) {
			return nil, nil, fmt.Errorf("%q no es una IP, CIDR ni dominio", e)
		}
		domains = append(domains, d)
	}
	set, err := NewIPSet(ips)
	return set, domains, err
}

// ParseIPList lee una IP o CIDR por línea; ignora vacías, comentarios y basura.
func ParseIPList(r io.Reader) (*IPSet, error) {
	s := &IPSet{addrs: make(map[netip.Addr]struct{})}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_ = s.add(line)
	}
	return s, sc.Err()
}

func (s *IPSet) add(e string) error {
	e = strings.TrimSpace(e)
	if strings.Contains(e, "/") {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			return fmt.Errorf("CIDR inválido %q", e)
		}
		p = p.Masked()
		if p.IsSingleIP() {
			s.addrs[p.Addr().Unmap()] = struct{}{}
		} else {
			s.prefixes = append(s.prefixes, p)
		}
		return nil
	}
	a, err := netip.ParseAddr(e)
	if err != nil {
		return fmt.Errorf("IP inválida %q", e)
	}
	s.addrs[a.Unmap()] = struct{}{}
	return nil
}

func (s *IPSet) Contains(a netip.Addr) bool {
	if s == nil {
		return false
	}
	a = a.Unmap()
	if _, ok := s.addrs[a]; ok {
		return true
	}
	for _, p := range s.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (s *IPSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.addrs) + len(s.prefixes)
}

// First devuelve una IP cualquiera del conjunto (para diagnósticos).
func (s *IPSet) First() (netip.Addr, bool) {
	if s != nil {
		for a := range s.addrs {
			if a.Is4() {
				return a, true
			}
		}
	}
	return netip.Addr{}, false
}

// Rangos publicados en https://www.cloudflare.com/ips/ (modo "cdn").
var cloudflareRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}
