package interactivequic

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

type Session struct {
	conn     *quic.Conn
	control  *quic.Stream
	stdin    *quic.SendStream
	pty      *quic.ReceiveStream
	ptyReady chan struct{}
	ptyErr   error

	sendMu   sync.Mutex
	eventCh  chan controlMessage
	eventErr chan error
	closeMu  sync.Mutex
	closed   bool
}

func Dial(ctx context.Context, endpoint, alpn, certPinSHA256, sessionID, sessionToken string) (*Session, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("missing quic endpoint")
	}
	alpn = strings.TrimSpace(alpn)
	if alpn == "" {
		return nil, errors.New("missing quic alpn")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, errors.New("missing session_id")
	}
	sessionToken = strings.TrimSpace(sessionToken)
	if sessionToken == "" {
		return nil, errors.New("missing session_token")
	}

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{alpn},
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyCertPin(rawCerts, certPinSHA256)
		},
	}

	conn, err := quic.DialAddr(ctx, endpoint, tlsCfg, &quic.Config{
		HandshakeIdleTimeout: 10 * time.Second,
		MaxIdleTimeout:       45 * time.Second,
		KeepAlivePeriod:      15 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("dial interactive endpoint %q: %w", endpoint, err)
	}

	controlStream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, fmt.Errorf("open control stream: %w", err)
	}

	encoder := json.NewEncoder(controlStream)
	decoder := json.NewDecoder(controlStream)
	if err := encoder.Encode(controlMessage{
		Type:         controlTypeHello,
		SessionID:    sessionID,
		SessionToken: sessionToken,
	}); err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, fmt.Errorf("send hello frame: %w", err)
	}

	var helloAck controlMessage
	if err := decoder.Decode(&helloAck); err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, fmt.Errorf("receive hello ack: %w", err)
	}
	if helloAck.Type == controlTypeError {
		_ = conn.CloseWithError(0, "")
		return nil, errors.New(strings.TrimSpace(helloAck.Error))
	}
	if helloAck.Type != controlTypeHelloAck {
		_ = conn.CloseWithError(0, "")
		return nil, fmt.Errorf("unexpected hello response type %q", helloAck.Type)
	}

	stdinStream, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, fmt.Errorf("open stdin stream: %w", err)
	}
	session := &Session{
		conn:     conn,
		control:  controlStream,
		stdin:    stdinStream,
		ptyReady: make(chan struct{}),
		eventCh:  make(chan controlMessage, 8),
		eventErr: make(chan error, 1),
	}
	go session.acceptPTY(conn.Context())
	go session.readControlEvents(decoder)
	return session, nil
}

func (s *Session) acceptPTY(ctx context.Context) {
	defer close(s.ptyReady)
	ptyStream, err := s.conn.AcceptUniStream(ctx)
	if err != nil {
		s.ptyErr = err
		return
	}
	s.pty = ptyStream
}

func verifyCertPin(rawCerts [][]byte, pin string) error {
	if len(rawCerts) == 0 {
		return errors.New("interactive cert pin check failed: missing peer certificate")
	}
	pin = normalizePin(pin)
	if pin == "" {
		return errors.New("interactive cert pin check failed: missing pin")
	}
	sum := sha256.Sum256(rawCerts[0])
	got := hex.EncodeToString(sum[:])
	if got != pin {
		return fmt.Errorf("interactive cert pin mismatch: got %s", got)
	}
	return nil
}

func normalizePin(pin string) string {
	pin = strings.TrimSpace(strings.ToLower(pin))
	pin = strings.TrimPrefix(pin, "sha256:")
	pin = strings.ReplaceAll(pin, ":", "")
	return pin
}

func (s *Session) Events() <-chan controlMessage {
	if s == nil {
		return nil
	}
	return s.eventCh
}

func (s *Session) EventErr() <-chan error {
	if s == nil {
		return nil
	}
	return s.eventErr
}

func (s *Session) ReadPTY(buf []byte) (int, error) {
	if s == nil {
		return 0, io.EOF
	}
	<-s.ptyReady
	if s.ptyErr != nil {
		return 0, s.ptyErr
	}
	if s.pty == nil {
		return 0, io.EOF
	}
	return s.pty.Read(buf)
}

func (s *Session) WriteStdin(data []byte) error {
	if s == nil || s.stdin == nil {
		return errors.New("interactive session is closed")
	}
	if len(data) == 0 {
		return nil
	}
	_, err := s.stdin.Write(data)
	return err
}

func (s *Session) CloseStdin() error {
	if s == nil || s.stdin == nil {
		return nil
	}
	return s.stdin.Close()
}

func (s *Session) SendResize(cols, rows uint32) error {
	return s.sendControl(controlMessage{
		Type: controlTypeResize,
		Cols: cols,
		Rows: rows,
	})
}

func (s *Session) SendSignal(signal int32) error {
	return s.sendControl(controlMessage{
		Type:   controlTypeSignal,
		Signal: signal,
	})
}

func (s *Session) SendClose(detach bool) error {
	return s.sendControl(controlMessage{
		Type:   controlTypeClose,
		Detach: detach,
	})
}

func (s *Session) sendControl(msg controlMessage) error {
	if s == nil || s.control == nil {
		return errors.New("interactive session is closed")
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return json.NewEncoder(s.control).Encode(msg)
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return nil
	}
	s.closed = true
	s.closeMu.Unlock()

	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.conn != nil {
		return s.conn.CloseWithError(0, "")
	}
	return nil
}

func (s *Session) readControlEvents(decoder *json.Decoder) {
	defer close(s.eventCh)
	for {
		var msg controlMessage
		if err := decoder.Decode(&msg); err != nil {
			if !errors.Is(err, io.EOF) {
				select {
				case s.eventErr <- err:
				default:
				}
			}
			close(s.eventErr)
			return
		}
		s.eventCh <- msg
	}
}
