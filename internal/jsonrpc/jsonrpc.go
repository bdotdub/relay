package jsonrpc

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
	"math"
	mrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bdotdub/relay/internal/logx"
)

type Client struct {
	writer    Writer
	pending   map[uint64]chan response
	pendingMu sync.Mutex
	nextID    atomic.Uint64
	notifyCh  chan Notification
	doneCh    chan struct{}
	closeOnce sync.Once
	closeFn   func() error
}

type Writer interface {
	WriteJSON(any) error
}

type response struct {
	result json.RawMessage
	err    error
}

type Notification struct {
	Method string
	Params json.RawMessage
}

type requestEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type notificationEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type responseEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *responseError  `json:"error,omitempty"`
}

type responseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *responseError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return fmt.Sprintf("json-rpc error %d", e.Code)
	}
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

func NewStdioClient(ctx context.Context, command []string) (*Client, error) {
	logx.Debugf("starting codex app-server %s", logx.KVSummary("command", strings.Join(command, " ")))
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
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

	client := &Client{
		writer:   &stdioJSONRPCWriter{writer: stdin},
		pending:  make(map[uint64]chan response),
		notifyCh: make(chan Notification, 256),
		doneCh:   make(chan struct{}),
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

func NewWebSocketClient(rawURL string) (*Client, error) {
	logx.Debugf("connecting codex websocket %s", logx.KVSummary("url", rawURL))
	conn, err := dialWebSocket(rawURL)
	if err != nil {
		return nil, err
	}
	client := &Client{
		writer:   conn,
		pending:  make(map[uint64]chan response),
		notifyCh: make(chan Notification, 256),
		doneCh:   make(chan struct{}),
		closeFn:  conn.Close,
	}
	go client.readWebSocket(conn)
	return client, nil
}

func (c *Client) Request(ctx context.Context, method string, params any, out any) error {
	requestID := c.nextID.Add(1)
	responseCh := make(chan response, 1)

	c.pendingMu.Lock()
	c.pending[requestID] = responseCh
	c.pendingMu.Unlock()

	payload := requestEnvelope{
		JSONRPC: "2.0",
		ID:      requestID,
		Method:  method,
		Params:  params,
	}
	logx.Debugf("jsonrpc request %s", summarizeOutgoingMessage(method, requestID, params))
	if err := c.writer.WriteJSON(payload); err != nil {
		c.removePending(requestID)
		return err
	}

	select {
	case response := <-responseCh:
		if response.err == nil {
			logx.Debugf("jsonrpc response %s", logx.KVSummary("method", method, "id", requestID))
		}
		if response.err != nil {
			return response.err
		}
		if out == nil || len(response.result) == 0 || string(response.result) == "null" {
			return nil
		}
		if err := json.Unmarshal(response.result, out); err != nil {
			return fmt.Errorf("decode JSON-RPC result %s: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.removePending(requestID)
		return ctx.Err()
	}
}

func (c *Client) Notify(method string, params any) error {
	payload := notificationEnvelope{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	logx.Debugf("jsonrpc notify %s", summarizeOutgoingMessage(method, nil, params))
	return c.writer.WriteJSON(payload)
}

func (c *Client) NextNotification(ctx context.Context) (Notification, error) {
	select {
	case notification, ok := <-c.notifyCh:
		if !ok {
			return Notification{}, io.EOF
		}
		return notification, nil
	case <-c.doneCh:
		return Notification{}, io.EOF
	case <-ctx.Done():
		return Notification{}, ctx.Err()
	}
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.doneCh)
		c.failPending(errors.New("codex transport closed"))
		err = c.closeFn()
	})
	return err
}

func (c *Client) readJSONLines(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := c.routeIncoming([]byte(line)); err != nil {
			logx.Warnf("failed to process JSON-RPC line: %v", err)
		}
	}
	c.failPending(errors.New("codex app-server stream closed"))
}

func (c *Client) readWebSocket(conn *webSocketConn) {
	for {
		payload, err := conn.ReadText()
		if err != nil {
			c.failPending(fmt.Errorf("codex websocket closed: %w", err))
			return
		}
		if err := c.routeIncoming(payload); err != nil {
			logx.Warnf("failed to process websocket JSON-RPC message: %v", err)
		}
	}
}

func (c *Client) routeIncoming(raw []byte) error {
	var payload responseEnvelope
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode JSON-RPC payload: %w", err)
	}
	if len(payload.ID) > 0 {
		id, ok := decodeResponseID(payload.ID)
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
		if payload.Error != nil {
			logx.Debugf("jsonrpc error %s", logx.KVSummary("id", id, "error", summarizeValue(payload.Error)))
			responseCh <- response{err: payload.Error}
			return nil
		}
		responseCh <- response{result: payload.Result}
		return nil
	}
	if payload.Method != "" {
		logx.Debugf("jsonrpc notification %s", summarizeIncomingNotification(payload))
		select {
		case <-c.doneCh:
		case c.notifyCh <- Notification{Method: payload.Method, Params: payload.Params}:
		}
	}
	return nil
}

func (c *Client) removePending(id uint64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, responseCh := range c.pending {
		responseCh <- response{err: err}
		delete(c.pending, id)
	}
}

type stdioJSONRPCWriter struct {
	writer io.Writer
	mu     sync.Mutex
}

func (w *stdioJSONRPCWriter) WriteJSON(payload any) error {
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
		logx.Debugf("%s: %s", prefix, scanner.Text())
	}
}

func summarizeOutgoingMessage(method string, id any, params any) string {
	summary := []any{"method", method, "id", id}
	if paramsSummary := summarizeParams(params); len(paramsSummary) > 0 {
		summary = append(summary, paramsSummary...)
	}
	return logx.KVSummary(summary...)
}

func summarizeIncomingNotification(payload responseEnvelope) string {
	summary := []any{"method", payload.Method}
	if paramsSummary := summarizeParams(payload.Params); len(paramsSummary) > 0 {
		summary = append(summary, paramsSummary...)
	}
	return logx.KVSummary(summary...)
}

func summarizeParams(raw any) []any {
	if raw == nil {
		return nil
	}

	switch typed := raw.(type) {
	case json.RawMessage:
		var params struct {
			ThreadID       string `json:"threadId"`
			TurnID         string `json:"turnId"`
			ExpectedTurnID string `json:"expectedTurnId"`
			Input          []struct {
				Text string `json:"text"`
			} `json:"input"`
		}
		if err := json.Unmarshal(typed, &params); err != nil {
			return nil
		}
		return summarizeParamFields(params.ThreadID, params.TurnID, params.ExpectedTurnID, params.Input)
	default:
		body, err := json.Marshal(raw)
		if err != nil {
			return nil
		}
		return summarizeParams(json.RawMessage(body))
	}
}

func summarizeParamFields(threadID string, turnID string, expectedTurnID string, input []struct {
	Text string `json:"text"`
}) []any {
	var summary []any
	if threadID != "" {
		summary = append(summary, "thread_id", threadID)
	}
	if turnID != "" {
		summary = append(summary, "turn_id", turnID)
	}
	if expectedTurnID != "" {
		summary = append(summary, "expected_turn_id", expectedTurnID)
	}
	if len(input) > 0 && input[0].Text != "" {
		summary = append(summary, "text", logx.SummarizeText(input[0].Text))
	}
	return summary
}

func summarizeValue(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if message, ok := typed["message"].(string); ok {
			return logx.SummarizeText(message)
		}
		return "object"
	case string:
		return logx.SummarizeText(typed)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func decodeResponseID(raw json.RawMessage) (uint64, bool) {
	var id uint64
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, true
	}

	var signed int64
	if err := json.Unmarshal(raw, &signed); err == nil {
		if signed < 0 {
			return 0, false
		}
		return uint64(signed), true
	}

	var floating float64
	if err := json.Unmarshal(raw, &floating); err == nil {
		return numberToUint64(floating)
	}
	return 0, false
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

func numberToInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed < math.MinInt64 || typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
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

func (c *webSocketConn) WriteJSON(payload any) error {
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
