//go:build ibkrprotocol

package protocoltest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// LiveRunner drives end-to-end probes against an IBKR Gateway using copies of
// the production encoding logic. The runner is gated behind the build tag
// "ibkrprotocol" so it is only compiled when explicitly requested.
type LiveRunner struct {
	Host                  string
	Port                  int
	ClientIDs             []int
	DialTimeout           time.Duration
	HandshakeFrames       []string
	Cases                 []MessageCase
	ReadTimeout           time.Duration
	UseTLS                bool
	EnableTLSFallback     bool
	TLSInsecureSkipVerify bool
	TLSServerName         string
}

// Run executes every message case using both encoding variants. It returns a
// slice of RunResult with request/response payloads and any observed errors.
func (r *LiveRunner) Run(ctx context.Context) ([]RunResult, error) {
	if len(r.Cases) == 0 {
		return nil, errors.New("no message cases provided")
	}
	if len(r.HandshakeFrames) == 0 {
		r.HandshakeFrames = []string{"v100..203", "v203"}
	}
	if len(r.ClientIDs) == 0 {
		r.ClientIDs = []int{101, 102, 103}
	}
	if r.DialTimeout == 0 {
		r.DialTimeout = 5 * time.Second
	}
	if r.ReadTimeout == 0 {
		r.ReadTimeout = 2 * time.Second
	}

	var results []RunResult

	clientIdx := 0
	for _, tc := range r.Cases {
		for _, includeNull := range []bool{true, false} {
			cid := r.ClientIDs[clientIdx%len(r.ClientIDs)]
			clientIdx++

			res := r.runSingleCase(ctx, tc, includeNull, cid)
			results = append(results, res)
		}
	}

	return results, nil
}

func (r *LiveRunner) tlsAttempts() []bool {
	base := r.UseTLS
	seq := []bool{base}
	if r.EnableTLSFallback {
		alt := !base
		if alt != base {
			seq = append(seq, alt)
		}
	}
	return seq
}

func (r *LiveRunner) dial(ctx context.Context, useTLS bool) (net.Conn, error) {
	addr := fmt.Sprintf("%s:%d", r.Host, r.Port)
	dialer := net.Dialer{Timeout: r.DialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if !useTLS {
		return conn, nil
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: r.TLSInsecureSkipVerify}
	serverName := r.TLSServerName
	if serverName == "" && !r.TLSInsecureSkipVerify {
		serverName = r.Host
	}
	if serverName != "" {
		tlsCfg.ServerName = serverName
	}
	tlsConn := tls.Client(conn, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func (r *LiveRunner) runSingleCase(ctx context.Context, tc MessageCase, includeNull bool, clientID int) RunResult {
	base := RunResult{Case: tc.Name, IncludeNull: includeNull, ClientID: clientID}
	handshakeHistory := make([]string, 0, len(r.HandshakeFrames))
	attempts := r.tlsAttempts()

	for idx, useTLS := range attempts {
		conn, err := r.dial(ctx, useTLS)
		if err != nil {
			base.Err = fmt.Errorf("dial (tls=%v): %w", useTLS, err)
			base.UsedTLS = useTLS
			if idx+1 < len(attempts) {
				continue
			}
			return base
		}

		res := r.executeCase(ctx, conn, tc, includeNull, clientID)
		_ = conn.Close()
		res.HandshakeErrors = append(handshakeHistory, res.HandshakeErrors...)
		res.UsedTLS = useTLS

		if res.Err == nil {
			return res
		}

		handshakeHistory = res.HandshakeErrors
		base = res
		if idx+1 == len(attempts) {
			return res
		}
	}

	return base
}

func (r *LiveRunner) executeCase(ctx context.Context, conn net.Conn, tc MessageCase, includeNull bool, clientID int) RunResult {
	res := RunResult{Case: tc.Name, IncludeNull: includeNull, ClientID: clientID}

	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	var serverVersion int
	for _, descriptor := range r.HandshakeFrames {
		if err := writeHandshake(conn, descriptor); err != nil {
			res.Err = fmt.Errorf("handshake send (%s): %w", descriptor, err)
			return res
		}
		version, connTime, err := readHandshake(conn, rw.Reader)
		if err == nil {
			serverVersion = version
			res.ServerVersion = version
			res.HandshakeTime = connTime
			break
		}
		res.HandshakeErrors = append(res.HandshakeErrors, fmt.Sprintf("%s: %v", descriptor, err))
	}
	if serverVersion == 0 {
		res.Err = fmt.Errorf("handshake failed after %d attempts", len(r.HandshakeFrames))
		return res
	}

	if tc.Name != "startAPI" {
		startFields := []interface{}{71, 2, clientID, ""}
		startPayload, err := EncodeMessage(serverVersion, includeNull, startFields...)
		if err != nil {
			res.Err = fmt.Errorf("startAPI encode: %w", err)
			return res
		}
		res.StartAPIRequestHex = hex.EncodeToString(startPayload)
		if err := sendPayload(conn, rw, startPayload); err != nil {
			res.Err = fmt.Errorf("startAPI send: %w", err)
			return res
		}
		if err := collectInitialMessages(conn, rw.Reader, r.ReadTimeout); err != nil {
			res.StartAPIResponseErr = err.Error()
		}
	}

	payload, err := EncodeMessage(serverVersion, includeNull, tc.Fields...)
	if err != nil {
		res.Err = fmt.Errorf("encode message: %w", err)
		return res
	}
	res.RequestHex = hex.EncodeToString(payload)

	if err := sendPayload(conn, rw, payload); err != nil {
		res.Err = fmt.Errorf("send payload: %w", err)
		return res
	}

	frames, err := collectFrames(conn, rw.Reader, r.ReadTimeout)
	if err != nil {
		res.ResponseErr = err.Error()
	}
	res.ResponseHex = frames

	return res
}

// RunResult captures the artifacts for a single message probe.
type RunResult struct {
	Case                string
	IncludeNull         bool
	ClientID            int
	ServerVersion       int
	HandshakeTime       string
	HandshakeErrors     []string
	StartAPIRequestHex  string
	StartAPIResponseErr string
	RequestHex          string
	ResponseHex         []string
	ResponseErr         string
	Err                 error
	UsedTLS             bool
}

func writeHandshake(conn net.Conn, descriptor string) error {
	frame := HandshakeFrame(descriptor)
	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	_, err := conn.Write(frame)
	return err
}

func readHandshake(conn net.Conn, r *bufio.Reader) (int, string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, "", err
	}
	defer conn.SetReadDeadline(time.Time{})

	if _, err := r.Peek(1); err != nil {
		return 0, "", err
	}
	head, err := r.Peek(4)
	if err != nil {
		return 0, "", err
	}
	first := head[0]
	if first == '-' || (first >= '0' && first <= '9') {
		return readAsciiHandshake(r)
	}
	return readLengthPrefixedHandshake(r)
}

func readAsciiHandshake(r *bufio.Reader) (int, string, error) {
	versionStr, err := readCString(r)
	if err != nil {
		return 0, "", err
	}
	version, err := strconv.Atoi(versionStr)
	if err != nil {
		return 0, "", fmt.Errorf("invalid version %q: %w", versionStr, err)
	}
	if version == -1 {
		redirect, _ := readCString(r)
		return 0, "", fmt.Errorf("handshake redirect: %s", redirect)
	}
	var connTime string
	if version >= 20 {
		connTime, err = readCString(r)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, "", err
		}
	}
	return version, connTime, nil
}

func readLengthPrefixedHandshake(r *bufio.Reader) (int, string, error) {
	var lengthBuf [4]byte
	if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
		return 0, "", err
	}
	frameLen := int(binary.BigEndian.Uint32(lengthBuf[:]))
	if frameLen <= 0 || frameLen > 4096 {
		return 0, "", fmt.Errorf("handshake frame length invalid: %d", frameLen)
	}

	payload := make([]byte, frameLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, "", err
	}

	segments := bytes.Split(payload, []byte{0})
	var fields []string
	for _, seg := range segments {
		if len(seg) == 0 {
			continue
		}
		fields = append(fields, string(seg))
	}
	if len(fields) == 0 {
		return 0, "", errors.New("empty handshake payload")
	}
	version, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, "", fmt.Errorf("invalid version %q: %w", fields[0], err)
	}
	if version == -1 {
		if len(fields) > 1 {
			return 0, "", fmt.Errorf("handshake redirect: %s", fields[1])
		}
		return 0, "", errors.New("handshake redirect (no target)")
	}
	connTime := ""
	if len(fields) > 1 {
		connTime = fields[1]
	}
	return version, connTime, nil
}

func readCString(r *bufio.Reader) (string, error) {
	data, err := r.ReadBytes('\x00')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(data), "\x00"), nil
}

func sendPayload(conn net.Conn, rw *bufio.ReadWriter, payload []byte) error {
	var lengthBuf [4]byte
	binary.BigEndian.PutUint32(lengthBuf[:], uint32(len(payload)))

	if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})

	if _, err := rw.Writer.Write(lengthBuf[:]); err != nil {
		return err
	}
	if _, err := rw.Writer.Write(payload); err != nil {
		return err
	}
	return rw.Writer.Flush()
}

func collectInitialMessages(conn net.Conn, r *bufio.Reader, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	defer conn.SetReadDeadline(time.Time{})
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return err
		}
		lengthBuf := make([]byte, 4)
		if _, err := io.ReadFull(r, lengthBuf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return nil
			}
			return err
		}
		frameLen := int(binary.BigEndian.Uint32(lengthBuf))
		if frameLen < 0 || frameLen > 1<<20 {
			return fmt.Errorf("invalid frame length %d", frameLen)
		}
		payload := make([]byte, frameLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return err
		}
		// Ignore contents; startAPI responses are logged by collectFrames later.
	}
}

func collectFrames(conn net.Conn, r *bufio.Reader, timeout time.Duration) ([]string, error) {
	deadline := time.Now().Add(timeout)
	defer conn.SetReadDeadline(time.Time{})
	var frames []string
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return frames, err
		}
		var lengthBuf [4]byte
		if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return frames, nil
			}
			return frames, err
		}
		frameLen := int(binary.BigEndian.Uint32(lengthBuf[:]))
		if frameLen <= 0 || frameLen > 1<<20 {
			return frames, fmt.Errorf("invalid frame size %d", frameLen)
		}
		payload := make([]byte, frameLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return frames, err
		}
		frames = append(frames, hex.EncodeToString(payload))
	}
}
