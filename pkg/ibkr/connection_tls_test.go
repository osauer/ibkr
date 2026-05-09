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
