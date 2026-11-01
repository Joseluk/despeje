package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const feedURL = "https://hayahora.futbol/estado/blocked-%s.txt"

// Blocklist mantiene las IPs que hayahora.futbol ve bloqueadas en este momento.
type Blocklist struct {
	URL    string
	Client *http.Client

	set atomic.Pointer[IPSet]

	mu      sync.Mutex
	etag    string
	updated time.Time
	lastErr error
}

func NewBlocklist(isp string) *Blocklist {
	return &Blocklist{URL: fmt.Sprintf(feedURL, isp), Client: http.DefaultClient}
}

func (b *Blocklist) Contains(a netip.Addr) bool { return b.set.Load().Contains(a) }

func (b *Blocklist) Set() *IPSet { return b.set.Load() }

func (b *Blocklist) Info() (count int, updated time.Time, lastErr error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.set.Load().Len(), b.updated, b.lastErr
}

func (b *Blocklist) Refresh(ctx context.Context) (changed bool, err error) {
	defer func() {
		b.mu.Lock()
		b.lastErr = err
		if err == nil {
			b.updated = time.Now()
		}
		b.mu.Unlock()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "despeje/"+version)
	b.mu.Lock()
	if b.etag != "" && b.set.Load() != nil {
		req.Header.Set("If-None-Match", b.etag)
	}
	b.mu.Unlock()

	resp, err := b.Client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return false, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("lista de bloqueos: HTTP %d", resp.StatusCode)
	}

	set, err := ParseIPList(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return false, err
	}
	b.set.Store(set)
	b.mu.Lock()
	b.etag = resp.Header.Get("ETag")
	b.mu.Unlock()
	return true, nil
}

func (b *Blocklist) Run(ctx context.Context, every time.Duration) {
	prev := -1
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		changed, err := b.Refresh(rctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("no se pudo actualizar la lista de bloqueos: %v", err)
			}
			return
		}
		if n := b.set.Load().Len(); changed && n != prev {
			if n == 0 {
				log.Printf("lista actualizada: no hay IPs bloqueadas ahora mismo")
			} else {
				log.Printf("lista actualizada: %d IPs bloqueadas ahora mismo", n)
			}
			prev = n
		}
	}

	refresh()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}
