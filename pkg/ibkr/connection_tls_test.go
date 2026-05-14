package ibkr

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func generateTestCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("x509 pair: %v", err)
	}
	return cert
}

func startFakeIBTLS(t *testing.T) (string, func()) {
	cert := generateTestCertificate(t)
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			go handleFakeIBConn(conn)
		}
	}()

	stop := func() {
		_ = ln.Close()
		<-done
	}

	return ln.Addr().String(), stop
}

func handleFakeIBConn(conn net.Conn) {
	defer conn.Close()
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	if string(header) != "API\x00" {
		return
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return
	}
	bodyLen := binary.BigEndian.Uint32(lenBuf[:])
	buf := make([]byte, bodyLen)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return
	}
	payload := []byte("203\x0020251009 12:00:00\x00")
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		return
	}
	if _, err := conn.Write(payload); err != nil {
		return
	}
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return
	}
	msgLen := binary.BigEndian.Uint32(lenBuf[:])
	msgPayload := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, msgPayload); err != nil {
		return
	}
	// Keep the connection open until the client disconnects.
	_, _ = io.Copy(io.Discard, conn)
}

// startWedgedTCP returns the address of a plain TCP listener that accepts
// connections and holds them open without speaking any protocol — the
// kernel-level analogue of an IB Gateway in the silent-after-clienthello
// state. Used to prove dialEndpoint's TLS handshake honors ctx cancel.
func startWedgedTCP(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var holds []net.Conn
		defer func() {
			for _, c := range holds {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			holds = append(holds, c) // hold open; never read or write
		}
	}()
	stop := func() {
		_ = ln.Close()
		<-done
	}
	return ln.Addr().String(), stop
}

// dialEndpoint must release its goroutine when ctx is cancelled mid-TLS-
// handshake. Pre-fix the code called tls.Conn.Handshake() (no ctx), which
// blocked on the read from the server until the kernel TCP timeout fired.
// The fix is HandshakeContext(ctx). This test proves the new behavior:
// against a TCP listener that accepts but never speaks TLS, dialEndpoint
// must return within ~200ms of ctx cancel, not the 10s+ Handshake default.
func TestDialEndpoint_TLSHandshakeRespectsContextCancel(t *testing.T) {
	addr, stop := startWedgedTCP(t)
	defer stop()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.ClientID = 91
	cfg.UseTLS = true
	cfg.TLSInsecureSkipVerify = true

	conn := NewConnection(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		err     error
		elapsed time.Duration
	}
	resCh := make(chan result, 1)

	start := time.Now()
	go func() {
		_, err := conn.dialEndpoint(ctx, true)
		resCh <- result{err: err, elapsed: time.Since(start)}
	}()

	// Give the dial enough time to land in the TLS handshake (TCP-level
	// connect to 127.0.0.1 is sub-ms; the wedge happens in HandshakeContext).
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case r := <-resCh:
		if r.err == nil {
			t.Fatal("expected error after ctx cancel, got nil")
		}
		if r.elapsed > 1*time.Second {
			t.Fatalf("dialEndpoint took %s after ctx cancel; HandshakeContext not honoring ctx", r.elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dialEndpoint did not return within 3s of ctx cancel — handshake still blocking")
	}
}

// TestIsHandshakeNoDataErr pins down the error flavors that should trigger
// TLS fallback. The macOS-latest CI runner produces ECONNRESET where Linux
// runners and local macOS produce EOF for the same plaintext-to-TLS-server
// scenario; the classifier must accept both, or TestConnectionTLSFallback
// becomes a portability flake.
func TestIsHandshakeNoDataErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not no-data", nil, false},
		{"EOF triggers fallback", io.EOF, true},
		{"unexpected EOF triggers fallback", io.ErrUnexpectedEOF, true},
		{"connection reset triggers fallback (Darwin RST flavor)", syscall.ECONNRESET, true},
		{"wrapped ECONNRESET still triggers fallback", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"net timeout triggers fallback", &timeoutErr{}, true},
		{"plain non-net error does not trigger", errors.New("nope"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHandshakeNoDataErr(tc.err); got != tc.want {
				t.Errorf("isHandshakeNoDataErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestConnectionTLSFallback(t *testing.T) {
	addr, stop := startFakeIBTLS(t)
	defer stop()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	cfg := DefaultConfig()
	cfg.Host = host
	cfg.Port = port
	cfg.ClientID = 90
	cfg.UseTLS = false
	cfg.EnableTLSFallback = true
	cfg.TLSInsecureSkipVerify = true

	conn := NewConnection(cfg)
	defer conn.Disconnect()

	if err := conn.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !conn.useTLS {
		t.Fatalf("expected TLS fallback, useTLS=%v", conn.useTLS)
	}
}
