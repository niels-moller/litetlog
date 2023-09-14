// Package bastion runs a reverse proxy service that allows un-addressable
// applications (for example those running behind a firewall or a NAT, or where
// the operator doesn't wish to take the DoS risk of being reachable from the
// Internet) to accept HTTP requests.
//
// Backends are identified by an Ed25519 public key, they authenticate with a
// self-signed TLS 1.3 certificate, and are reachable at a sub-path prefixed by
// the key hash.
//
// Read more at
// https://git.glasklar.is/sigsum/project/documentation/-/blob/main/bastion.md.
package bastion

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// Config provides parameters for a new Bastion.
type Config struct {
	// GetCertificate returns the certificate for bastion backend connections.
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)

	// AllowedBackend returns whether the backend is allowed to
	// serve requests. It's passed the hash of its Ed25519 public key.
	//
	// AllowedBackend may be called concurrently.
	AllowedBackend func(keyHash [sha256.Size]byte) bool

	// Log is used to log backend connections and errors in forwarding requests.
	// If nil, [log.Default] is used.
	Log *log.Logger
}

// A Bastion keeps track of backend connections, and serves HTTP requests by
// routing them to the matching backend.
type Bastion struct {
	c     *Config
	proxy *httputil.ReverseProxy
	pool  *backendConnectionsPool
}

type keyHash [sha256.Size]byte

// New returns a new Bastion.
//
// The Config must not be modified after the call to New.
func New(c *Config) (*Bastion, error) {
	b := &Bastion{c: c}
	b.pool = &backendConnectionsPool{
		log:   log.Default(),
		conns: make(map[keyHash]*http2.ClientConn),
	}
	if c.Log != nil {
		b.pool.log = c.Log
	}
	b.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https" // needed for the required :scheme header
			pr.Out.Host = pr.In.Context().Value("backend").(string)
			pr.SetXForwarded()
			// We don't interpret the query, so pass it on unmodified.
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
		},
		Transport: b.pool,
		ErrorLog:  c.Log,
	}
	return b, nil
}

// ConfigureServer sets up srv to handle backend connections to the bastion. It
// wraps TLSConfig.GetConfigForClient to intercept backend connections, and sets
// TLSNextProto for the bastion ALPN protocol. The original tls.Config is still
// used for non-bastion backend connections.
//
// Note that since TLSNextProto won't be nil after a call to ConfigureServer,
// the caller might want to call [http2.ConfigureServer] as well.
func (b *Bastion) ConfigureServer(srv *http.Server) error {
	if srv.TLSNextProto == nil {
		srv.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	}
	srv.TLSNextProto["bastion/0"] = b.pool.handleBackend

	bastionTLSConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"bastion/0"},
		ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			leaf := cs.PeerCertificates[0]
			pk, ok := leaf.PublicKey.(ed25519.PublicKey)
			if !ok {
				return errors.New("self-signed certificate key type is not Ed25519")
			}
			h := sha256.Sum256(pk)
			if !b.c.AllowedBackend(h) {
				return fmt.Errorf("unrecognized backend %x", h)
			}
			return nil
		},
		GetCertificate: b.c.GetCertificate,
	}

	if srv.TLSConfig == nil {
		srv.TLSConfig = &tls.Config{}
	}
	oldGetConfigForClient := srv.TLSConfig.GetConfigForClient
	srv.TLSConfig.GetConfigForClient = func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		for _, proto := range chi.SupportedProtos {
			if proto == "bastion/0" {
				// This is a bastion connection from a backend.
				return bastionTLSConfig, nil
			}
		}
		if oldGetConfigForClient != nil {
			return oldGetConfigForClient(chi)
		}
		return nil, nil
	}

	return nil
}

// ServeHTTP serves requests rooted at "/<hex key hash>/" by routing them to the
// backend that authenticated with that key. Other requests are served a 404 Not
// Found status.
func (b *Bastion) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/") {
		http.Error(w, "request must start with /KEY_HASH/", http.StatusNotFound)
		return
	}
	path = path[1:]
	kh, path, ok := strings.Cut(path, "/")
	if !ok {
		http.Error(w, "request must start with /KEY_HASH/", http.StatusNotFound)
		return
	}
	ctx := context.WithValue(r.Context(), "backend", kh)
	r = r.Clone(ctx)
	r.URL.Path = "/" + path
	b.proxy.ServeHTTP(w, r)
}

type backendConnectionsPool struct {
	log *log.Logger
	sync.RWMutex
	conns map[keyHash]*http2.ClientConn
}

func (p *backendConnectionsPool) RoundTrip(r *http.Request) (*http.Response, error) {
	kh, err := hex.DecodeString(r.Host)
	if err != nil || len(kh) != sha256.Size {
		// TODO: return this as a response instead.
		return nil, errors.New("invalid backend key hash")
	}
	p.RLock()
	cc, ok := p.conns[keyHash(kh)]
	p.RUnlock()
	if !ok {
		// TODO: return this as a response instead.
		return nil, errors.New("backend unavailable")
	}
	return cc.RoundTrip(r)
}

func (p *backendConnectionsPool) handleBackend(hs *http.Server, c *tls.Conn, h http.Handler) {
	backend := sha256.Sum256(c.ConnectionState().PeerCertificates[0].PublicKey.(ed25519.PublicKey))
	t := &http2.Transport{
		// Send a PING every 15s, with the default 15s timeout.
		ReadIdleTimeout: 15 * time.Second,
	}
	cc, err := t.NewClientConn(c)
	if err != nil {
		p.log.Printf("%x: failed to convert to HTTP/2 client connection: %v", backend, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cc.Ping(ctx); err != nil {
		p.log.Printf("%x: did not respond to PING: %v", backend, err)
		return
	}

	p.Lock()
	if oldCC, ok := p.conns[backend]; ok && !oldCC.State().Closed {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			oldCC.Shutdown(ctx)
		}()
	}
	p.conns[backend] = cc
	p.Unlock()

	p.log.Printf("%x: accepted new backend connection", backend)
	// We need not to return, or http.Server will close this connection. There
	// is no way to wait for the ClientConn's closing, so we poll. We could
	// switch this to a Server.ConnState callback with some plumbing.
	for !cc.State().Closed {
		time.Sleep(1 * time.Second)
	}
	p.log.Printf("%x: backend connection expired", backend)
}
