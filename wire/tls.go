package wire

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
)

// TLS on the wire protects the connection itself: confidentiality and integrity in transit,
// and, with a client CA configured, the mutual-TLS handshake that backs X.509 authentication
// (spec 2061 doc 16 §9, doc 17 §12). All of it is standard-library crypto/tls and
// crypto/x509, no cgo. TLS 1.2 is the floor; 1.0 and 1.1 are refused.

// TLSMode mirrors the four MongoDB TLS modes (spec 2061 doc 17 §12.2).
type TLSMode string

const (
	// TLSDisabled accepts only plaintext, acceptable for a loopback-only listener.
	TLSDisabled TLSMode = "disabled"
	// TLSAllow accepts both TLS and plaintext on the same port.
	TLSAllow TLSMode = "allowTLS"
	// TLSPrefer accepts both but is intended to favor TLS; at a single node it behaves
	// like allowTLS.
	TLSPrefer TLSMode = "preferTLS"
	// TLSRequire refuses any connection that does not complete a TLS handshake, the
	// recommended setting for a networked server.
	TLSRequire TLSMode = "requireTLS"
)

// TLSOptions configures the listener's TLS. The zero value (empty Mode) leaves TLS off.
type TLSOptions struct {
	Mode     TLSMode
	CertFile string // PEM server certificate (may be a chain)
	KeyFile  string // PEM server private key
	CAFile   string // PEM CA bundle that signs trusted client certificates (enables mTLS)
	// RequireClientCert demands a client certificate on every TLS connection. X.509
	// authentication needs a verified client cert, so this is implied once CAFile is set
	// and a connection wants to use MONGODB-X509.
	RequireClientCert bool
}

// buildTLSConfig turns the options into a crypto/tls server config, or returns nil when TLS
// is off. A CAFile turns on client-certificate verification so a presented cert is checked
// against the bundle before X.509 auth trusts its subject.
func buildTLSConfig(opts TLSOptions) (*tls.Config, error) {
	if opts.Mode == "" || opts.Mode == TLSDisabled {
		return nil, nil
	}
	if opts.CertFile == "" || opts.KeyFile == "" {
		return nil, errors.New("wire: TLS requires both a certificate and a key file")
	}
	cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("wire: load TLS keypair: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"mongodb"},
	}
	if opts.CAFile != "" {
		pool, err := loadCAPool(opts.CAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		// Verify a presented cert against the pool, but still let a client connect without
		// one (so SCRAM users keep working over the same listener). X.509 auth later
		// insists on a verified cert being present.
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
		if opts.RequireClientCert {
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}
	return cfg, nil
}

// loadCAPool reads a PEM CA bundle into a cert pool.
func loadCAPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("wire: read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("wire: no certificates found in %s", caFile)
	}
	return pool, nil
}

// wrapListener applies the TLS mode to a raw listener. requireTLS hands every connection to
// the TLS server; allow/prefer sniff the first byte and start TLS only for a TLS
// ClientHello, leaving plaintext connections untouched; disabled returns the listener
// unchanged.
func wrapListener(ln net.Listener, cfg *tls.Config, mode TLSMode) net.Listener {
	if cfg == nil || mode == "" || mode == TLSDisabled {
		return ln
	}
	return &tlsListener{Listener: ln, cfg: cfg, mode: mode}
}

// tlsListener applies the TLS mode at Accept time.
type tlsListener struct {
	net.Listener
	cfg  *tls.Config
	mode TLSMode
}

// tlsRecordHandshake is the first byte of a TLS record carrying a handshake (ContentType
// 22), how the sniffing modes tell a TLS ClientHello from a plaintext wire message.
const tlsRecordHandshake = 0x16

func (l *tlsListener) Accept() (net.Conn, error) {
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.mode == TLSRequire {
			// Hand the raw connection to the TLS server; the handshake runs lazily on the
			// first read, and a plaintext client fails it and is dropped, which is the
			// rejection requireTLS promises.
			return tls.Server(raw, l.cfg), nil
		}
		// allow/prefer: peek the first byte without consuming it.
		pc := &prefixConn{Conn: raw, r: bufio.NewReader(raw)}
		first, err := pc.r.Peek(1)
		if err != nil {
			_ = raw.Close()
			continue
		}
		if first[0] == tlsRecordHandshake {
			return tls.Server(pc, l.cfg), nil
		}
		return pc, nil
	}
}

// prefixConn lets a peeked byte be read again. It wraps the raw connection with a buffered
// reader that already holds the sniffed bytes, so the wire reader downstream sees the full
// stream.
type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }
