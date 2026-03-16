package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

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
	WriteJSON(context.Context, any) error
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
	logx.Debug("starting codex app-server", "command", strings.Join(command, " "))
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

func NewWebSocketClient(ctx context.Context, rawURL string) (*Client, error) {
	logx.Debug("connecting codex websocket", "url", rawURL)
	conn, _, err := websocket.Dial(ctx, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial websocket: %w", err)
	}
	client := &Client{
		writer:   &webSocketJSONRPCWriter{conn: conn},
		pending:  make(map[uint64]chan response),
		notifyCh: make(chan Notification, 256),
		doneCh:   make(chan struct{}),
		closeFn: func() error {
			return conn.Close(websocket.StatusNormalClosure, "")
		},
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
	logx.Debug("jsonrpc request", summarizeOutgoingMessage(method, requestID, params)...)
	if err := c.writer.WriteJSON(ctx, payload); err != nil {
		c.removePending(requestID)
		return err
	}

	select {
	case response := <-responseCh:
		if response.err == nil {
			logx.Debug("jsonrpc response", "method", method, "id", requestID)
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
	logx.Debug("jsonrpc notify", summarizeOutgoingMessage(method, nil, params)...)
	return c.writer.WriteJSON(context.Background(), payload)
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
			logx.Warn("failed to process JSON-RPC line", "error", err)
		}
	}
	c.failPending(errors.New("codex app-server stream closed"))
}

func (c *Client) readWebSocket(conn *websocket.Conn) {
	for {
		var payload responseEnvelope
		err := wsjson.Read(context.Background(), conn, &payload)
		if err != nil {
			if errors.Is(err, context.Canceled) || isExpectedWebSocketClose(err) {
				return
			}
			c.failPending(fmt.Errorf("codex websocket closed: %w", err))
			return
		}
		if err := c.routeIncomingEnvelope(payload); err != nil {
			logx.Warn("failed to process websocket JSON-RPC message", "error", err)
		}
	}
}

func (c *Client) routeIncoming(raw []byte) error {
	var payload responseEnvelope
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode JSON-RPC payload: %w", err)
	}
	return c.routeIncomingEnvelope(payload)
}

func (c *Client) routeIncomingEnvelope(payload responseEnvelope) error {
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
			logx.Debug("jsonrpc error", "id", id, "error", payload.Error.Error())
			responseCh <- response{err: payload.Error}
			return nil
		}
		responseCh <- response{result: payload.Result}
		return nil
	}
	if payload.Method != "" {
		logx.Debug("jsonrpc notification", summarizeIncomingNotification(payload)...)
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

func (w *stdioJSONRPCWriter) WriteJSON(_ context.Context, payload any) error {
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

type webSocketJSONRPCWriter struct {
	conn *websocket.Conn
}

func (w *webSocketJSONRPCWriter) WriteJSON(ctx context.Context, payload any) error {
	if err := wsjson.Write(ctx, w.conn, payload); err != nil {
		return fmt.Errorf("write websocket JSON-RPC payload: %w", err)
	}
	return nil
}

func logLines(reader io.Reader, prefix string) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		logx.Debug("subprocess output", "stream", prefix, "line", scanner.Text())
	}
}

func summarizeOutgoingMessage(method string, id any, params any) []any {
	summary := []any{"method", method, "id", id}
	if paramsSummary := summarizeParams(params); len(paramsSummary) > 0 {
		summary = append(summary, paramsSummary...)
	}
	return summary
}

func summarizeIncomingNotification(payload responseEnvelope) []any {
	summary := []any{"method", payload.Method}
	if paramsSummary := summarizeParams(payload.Params); len(paramsSummary) > 0 {
		summary = append(summary, paramsSummary...)
	}
	return summary
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

func isExpectedWebSocketClose(err error) bool {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	default:
		return false
	}
}
