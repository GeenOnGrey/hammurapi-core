package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// JSON-RPC 2.0 over newline-delimited JSON on stdio.

type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// ErrClosed is returned for calls on a dead connection.
var ErrClosed = errors.New("acp: connection closed")

type conn struct {
	w       io.Writer
	wmu     sync.Mutex
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcMessage
	closed  chan struct{}
	err     error

	onNotify  func(method string, params json.RawMessage)
	onRequest func(method string, params json.RawMessage) (any, *RPCError)
}

func newConn(r io.Reader, w io.Writer,
	onNotify func(string, json.RawMessage),
	onRequest func(string, json.RawMessage) (any, *RPCError)) *conn {
	c := &conn{w: w, pending: map[int64]chan rpcMessage{}, closed: make(chan struct{}), onNotify: onNotify, onRequest: onRequest}
	go c.readLoop(r)
	return c
}

func (c *conn) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m rpcMessage
		if err := json.Unmarshal(line, &m); err != nil {
			continue // agents may log non-JSON lines; ignore them
		}
		switch {
		case m.Method != "" && m.ID != nil:
			go c.handleRequest(m)
		case m.Method != "":
			if c.onNotify != nil {
				c.onNotify(m.Method, m.Params)
			}
		case m.ID != nil:
			var id int64
			if json.Unmarshal(*m.ID, &id) == nil {
				c.mu.Lock()
				ch := c.pending[id]
				delete(c.pending, id)
				c.mu.Unlock()
				if ch != nil {
					ch <- m
				}
			}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.close(err)
}

func (c *conn) close(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return
	default:
	}
	c.err = err
	close(c.closed)
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

func (c *conn) handleRequest(m rpcMessage) {
	var result any
	var rerr *RPCError
	if c.onRequest != nil {
		result, rerr = c.onRequest(m.Method, m.Params)
	} else {
		rerr = &RPCError{Code: -32601, Message: "method not found"}
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	_ = c.write(resp)
}

func (c *conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// call sends a request and waits for its response.
func (c *conn) call(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan rpcMessage, 1)
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return ErrClosed
	default:
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case m, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	}
}

// notify sends a notification.
func (c *conn) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
