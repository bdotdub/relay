package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

type jsonRPCClient struct {
	writer    jsonRPCWriter
	pending   map[uint64]chan jsonRPCResponse
	pendingMu sync.Mutex
	nextID    atomic.Uint64
	notifyCh  chan map[string]any
	closeOnce sync.Once
	closeFn   func() error
}

type jsonRPCWriter interface {
	WriteJSON(map[string]any) error
}

type jsonRPCResponse struct {
	result map[string]any
	err    error
}

func newJSONRPCStdioClient(command []string) (*jsonRPCClient, error) {
	cmd := exec.Command(command[0], command[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("capture codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capture codex stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("capture codex stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex app server: %w", err)
	}

	client := &jsonRPCClient{
		writer:   &stdioJSONRPCWriter{writer: stdin},
		pending:  make(map[uint64]chan jsonRPCResponse),
		notifyCh: make(chan map[string]any, 256),
		closeFn: func() error {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
			return nil
		},
	}

	go client.readJSONLines(stdout)
	go logLines(stderr, "codex-app-server")
	return client, nil
}

func newJSONRPCWebSocketClient(rawURL string) (*jsonRPCClient, error) {
	conn, err := dialWebSocket(rawURL)
	if err != nil {
		return nil, err
	}
	client := &jsonRPCClient{
		writer:   conn,
		pending:  make(map[uint64]chan jsonRPCResponse),
		notifyCh: make(chan map[string]any, 256),
		closeFn:  conn.Close,
	}
	go client.readWebSocket(conn)
	return client, nil
}

func (c *jsonRPCClient) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	requestID := c.nextID.Add(1)
	responseCh := make(chan jsonRPCResponse, 1)

	c.pendingMu.Lock()
	c.pending[requestID] = responseCh
	c.pendingMu.Unlock()

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  method,
		"params":  params,
	}
	if err := c.writer.WriteJSON(payload); err != nil {
		c.removePending(requestID)
		return nil, err
	}

	select {
	case response := <-responseCh:
		return response.result, response.err
	case <-ctx.Done():
		c.removePending(requestID)
		return nil, ctx.Err()
	}
}

func (c *jsonRPCClient) notify(method string, params map[string]any) error {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	return c.writer.WriteJSON(payload)
}

func (c *jsonRPCClient) nextNotification(ctx context.Context) (map[string]any, error) {
	select {
	case notification, ok := <-c.notifyCh:
		if !ok {
			return nil, io.EOF
		}
		return notification, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *jsonRPCClient) close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.closeFn()
		close(c.notifyCh)
		c.failPending(errors.New("codex transport closed"))
	})
	return err
}

func (c *jsonRPCClient) readJSONLines(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := c.routeIncoming([]byte(line)); err != nil {
			log.Printf("failed to process JSON-RPC line: %v", err)
		}
	}
	c.failPending(errors.New("codex app-server stream closed"))
}

func (c *jsonRPCClient) readWebSocket(conn *webSocketConn) {
	for {
		payload, err := conn.ReadText()
		if err != nil {
			c.failPending(fmt.Errorf("codex websocket closed: %w", err))
			return
		}
		if err := c.routeIncoming(payload); err != nil {
			log.Printf("failed to process websocket JSON-RPC message: %v", err)
		}
	}
}

func (c *jsonRPCClient) routeIncoming(raw []byte) error {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode JSON-RPC payload: %w", err)
	}
	if idValue, ok := payload["id"]; ok {
		id, ok := numberToUint64(idValue)
		if !ok {
			return errors.New("received JSON-RPC response with non-numeric id")
		}
		c.pendingMu.Lock()
		responseCh := c.pending[id]
		delete(c.pending, id)
		c.pendingMu.Unlock()
		if responseCh == nil {
			return nil
		}
		if errorValue, ok := payload["error"]; ok {
			responseCh <- jsonRPCResponse{err: fmt.Errorf("json-rpc error: %v", errorValue)}
			return nil
		}
		result, _ := payload["result"].(map[string]any)
		if result == nil {
			result = map[string]any{}
		}
		responseCh <- jsonRPCResponse{result: result}
		return nil
	}
	if _, ok := payload["method"]; ok {
		c.notifyCh <- payload
	}
	return nil
}

func (c *jsonRPCClient) removePending(id uint64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *jsonRPCClient) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, responseCh := range c.pending {
		responseCh <- jsonRPCResponse{err: err}
		delete(c.pending, id)
	}
}

type stdioJSONRPCWriter struct {
	writer io.Writer
	mu     sync.Mutex
}

func (w *stdioJSONRPCWriter) WriteJSON(payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal JSON-RPC payload: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.writer.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("write JSON-RPC payload: %w", err)
	}
	return nil
}

func logLines(reader io.Reader, prefix string) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		log.Printf("%s: %s", prefix, scanner.Text())
	}
}

func numberToUint64(value any) (uint64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed < 0 || typed > math.MaxUint64 {
			return 0, false
		}
		return uint64(typed), true
	case int:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case uint64:
		return typed, true
	default:
		return 0, false
	}
}

type webSocketConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func dialWebSocket(rawURL string) (*webSocketConn, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse websocket url: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("unsupported websocket scheme %q", parsed.Scheme)
	}

	host := parsed.Host
	if !strings.Contains(host, ":") {
		if parsed.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	var conn net.Conn
	if parsed.Scheme == "wss" {
		conn, err = tls.Dial("tcp", host, &tls.Config{
			ServerName: strings.Split(parsed.Host, ":")[0],
		})
	} else {
		conn, err = net.Dial("tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("dial websocket: %w", err)
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("generate websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := parsed.RequestURI()
	if path == "" {
		path = "/"
	}

	request := fmt.Sprintf("GET %s HTTP/1.1\r\n", path) +
		fmt.Sprintf("Host: %s\r\n", parsed.Host) +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		fmt.Sprintf("Sec-WebSocket-Key: %s\r\n", key) +
		"Sec-WebSocket-Version: 13\r\n\r\n"

	if _, err := io.WriteString(conn, request); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write websocket handshake: %w", err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{
		Method: http.MethodGet,
		URL:    parsed,
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read websocket handshake: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("websocket handshake failed: %s", response.Status)
	}
	expectedAccept := computeWebSocketAccept(key)
	if response.Header.Get("Sec-WebSocket-Accept") != expectedAccept {
		_ = conn.Close()
		return nil, errors.New("websocket handshake returned invalid Sec-WebSocket-Accept")
	}

	return &webSocketConn{
		conn: &bufferedConn{
			Conn:   conn,
			reader: reader,
		},
	}, nil
}

func computeWebSocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func (c *webSocketConn) Close() error {
	return c.conn.Close()
}

func (c *webSocketConn) WriteJSON(payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal websocket JSON-RPC payload: %w", err)
	}
	return c.writeFrame(0x1, body)
}

func (c *webSocketConn) ReadText() ([]byte, error) {
	for {
		opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 0x1:
			return payload, nil
		case 0x8:
			return nil, io.EOF
		case 0x9:
			if err := c.writeFrame(0xA, payload); err != nil {
				return nil, err
			}
		case 0xA:
		default:
		}
	}
}

func (c *webSocketConn) writeFrame(opcode byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	header := []byte{0x80 | opcode}
	maskBit := byte(0x80)
	length := len(payload)

	switch {
	case length < 126:
		header = append(header, maskBit|byte(length))
	case length <= math.MaxUint16:
		header = append(header, maskBit|126)
		extra := make([]byte, 2)
		binary.BigEndian.PutUint16(extra, uint16(length))
		header = append(header, extra...)
	default:
		header = append(header, maskBit|127)
		extra := make([]byte, 8)
		binary.BigEndian.PutUint64(extra, uint64(length))
		header = append(header, extra...)
	}

	maskKey := [4]byte{}
	mrand.Read(maskKey[:])
	header = append(header, maskKey[:]...)

	masked := make([]byte, len(payload))
	for index, value := range payload {
		masked[index] = value ^ maskKey[index%4]
	}

	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	if _, err := c.conn.Write(masked); err != nil {
		return err
	}
	return nil
}

func (c *webSocketConn) readFrame() (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(c.conn, header); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	payloadLength := uint64(header[1] & 0x7f)
	switch payloadLength {
	case 126:
		extra := make([]byte, 2)
		if _, err := io.ReadFull(c.conn, extra); err != nil {
			return 0, nil, err
		}
		payloadLength = uint64(binary.BigEndian.Uint16(extra))
	case 127:
		extra := make([]byte, 8)
		if _, err := io.ReadFull(c.conn, extra); err != nil {
			return 0, nil, err
		}
		payloadLength = binary.BigEndian.Uint64(extra)
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.conn, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, payloadLength)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for index := range payload {
			payload[index] ^= maskKey[index%4]
		}
	}
	return opcode, payload, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
