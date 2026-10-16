package webapp

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

// cdpClient speaks the Chrome DevTools Protocol over Chrome's --remote-debugging-pipe: JSON messages
// separated by a NUL byte. A pipe is used instead of --remote-debugging-port so nothing else on the
// gateway host can attach to the browser; CDP can read cookies and run script in the page.

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type cdpEvent struct {
	Method    string
	Params    json.RawMessage
	SessionID string
}

type cdpResponse struct {
	result json.RawMessage
	err    error
}

type cdpClient struct {
	writeMu sync.Mutex
	w       io.Writer

	nextID    atomic.Int64
	pendingMu sync.Mutex
	pending   map[int64]chan cdpResponse

	// Event delivery is decoupled from the read loop so a slow consumer can never stop command responses
	// from being read. Screencast frames are acked one at a time, so the queue stays short in practice.
	events chan cdpEvent
	done   chan struct{}
	stop   chan struct{}
	err    error
}

func newCDPClient(r io.Reader, w io.Writer) *cdpClient {
	c := &cdpClient{
		w:       w,
		pending: make(map[int64]chan cdpResponse),
		events:  make(chan cdpEvent, 256),
		done:    make(chan struct{}),
		stop:    make(chan struct{}),
	}
	go c.readLoop(r)
	return c
}

func (c *cdpClient) readLoop(r io.Reader) {
	reader := bufio.NewReaderSize(r, 1<<20)
	defer func() {
		c.pendingMu.Lock()
		for id, ch := range c.pending {
			ch <- cdpResponse{err: errors.New("browser connection closed")}
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
		close(c.done)
	}()

	for {
		raw, err := reader.ReadBytes(0)
		if err != nil {
			c.err = err
			return
		}
		raw = raw[:len(raw)-1]

		var msg cdpMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		if msg.ID != 0 {
			c.pendingMu.Lock()
			ch, ok := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.pendingMu.Unlock()
			if !ok {
				continue
			}
			if msg.Error != nil {
				ch <- cdpResponse{err: fmt.Errorf("cdp error %d: %s", msg.Error.Code, msg.Error.Message)}
			} else {
				ch <- cdpResponse{result: msg.Result}
			}
			continue
		}

		if msg.Method != "" {
			select {
			case c.events <- cdpEvent{Method: msg.Method, Params: msg.Params, SessionID: msg.SessionID}:
			case <-c.stop:
				return
			}
		}
	}
}

func (c *cdpClient) write(msg cdpMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(append(data, 0))
	return err
}

// close stops event delivery so the read loop can't block on a consumer that has gone away.
func (c *cdpClient) close() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
}

// call sends a command and waits for its response.
func (c *cdpClient) call(ctx context.Context, sessionID, method string, params any, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan cdpResponse, 1)

	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	var rawParams json.RawMessage
	if params != nil {
		var err error
		if rawParams, err = json.Marshal(params); err != nil {
			return err
		}
	}

	if err := c.write(cdpMessage{ID: id, Method: method, Params: rawParams, SessionID: sessionID}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return fmt.Errorf("cdp %s: %w", method, err)
	}

	select {
	case resp := <-ch:
		if resp.err != nil {
			return fmt.Errorf("cdp %s: %w", method, resp.err)
		}
		if result != nil && len(resp.result) > 0 {
			return json.Unmarshal(resp.result, result)
		}
		return nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return ctx.Err()
	}
}

// notify sends a command without waiting for its response, for calls like screencast acks issued from the
// event loop, where waiting would stall event delivery.
func (c *cdpClient) notify(sessionID, method string, params any) error {
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(cdpMessage{ID: c.nextID.Add(1), Method: method, Params: rawParams, SessionID: sessionID})
}
