package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Infisical/infisical-merge/packages/pam/session"
)

func TestFrameRoundTrip(t *testing.T) {
	meta := FrameMeta{Seq: 7, Ts: 1.5, Width: 1280, Height: 800}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0x01, 0x02}

	payload, err := encodeFrame(meta, jpeg)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeMessage(&buf, MessageTypeFrame, payload); err != nil {
		t.Fatal(err)
	}

	msg, err := readMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != MessageTypeFrame {
		t.Fatalf("type = %d, want %d", msg.Type, MessageTypeFrame)
	}
	gotMeta, gotJPEG, err := decodeFrame(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if gotMeta != meta || !bytes.Equal(gotJPEG, jpeg) {
		t.Fatalf("round trip mismatch: %+v %x", gotMeta, gotJPEG)
	}
}

func TestReadMessageRejectsOversizedLength(t *testing.T) {
	header := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := readMessage(bytes.NewReader(header)); err == nil {
		t.Fatal("expected an error for a length over the limit")
	}
}

func TestValidateTargetURL(t *testing.T) {
	for _, raw := range []string{"http://localhost:3001", "https://grafana.internal/dashboards"} {
		if _, err := validateTargetURL(raw); err != nil {
			t.Errorf("%q: unexpected error %v", raw, err)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "javascript:alert(1)", "http://", "chrome://settings"} {
		if _, err := validateTargetURL(raw); err == nil {
			t.Errorf("%q: expected an error", raw)
		}
	}
}

// Launches a real browser against a local page and checks the viewer receives a navigation and a JPEG
// frame, then that a viewer disconnect ends the session cleanly.
func TestHandleConnectionStreamsFrames(t *testing.T) {
	if !IsSupported() {
		t.Skip("no browser installed")
	}

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body style="background:#0a7"><h1>internal app</h1></body></html>`))
	}))
	defer app.Close()

	viewer, gateway := net.Pipe()
	proxy := NewWebAppProxy(WebAppProxyConfig{TargetURL: app.URL, SessionID: "test-session"})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- proxy.HandleConnection(ctx, gateway) }()

	var gotReady, gotNavigated, gotFrame bool
	_ = viewer.SetReadDeadline(time.Now().Add(20 * time.Second))
	for !(gotReady && gotNavigated && gotFrame) {
		msg, err := readMessage(viewer)
		if err != nil {
			var handlerErr error
			select {
			case handlerErr = <-done:
			case <-time.After(time.Second):
			}
			t.Fatalf("read: %v (ready=%v navigated=%v frame=%v, handler: %v)", err, gotReady, gotNavigated, gotFrame, handlerErr)
		}
		switch msg.Type {
		case MessageTypeEvent:
			var event Event
			if err := json.Unmarshal(msg.Payload, &event); err != nil {
				t.Fatal(err)
			}
			switch event.Kind {
			case EventKindReady:
				gotReady = event.Width == defaultViewportWidth && event.Height == defaultViewportHeight
			case EventKindNavigated:
				gotNavigated = gotNavigated || event.URL == app.URL+"/"
			}
		case MessageTypeFrame:
			meta, jpeg, err := decodeFrame(msg.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if len(jpeg) < 2 || jpeg[0] != 0xFF || jpeg[1] != 0xD8 {
				t.Fatalf("frame %d is not a JPEG", meta.Seq)
			}
			gotFrame = meta.Seq >= 1 && meta.Width > 0
		}
	}

	viewer.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("HandleConnection returned %v after viewer disconnect", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HandleConnection did not return after viewer disconnect")
	}
}

// viewerSession drives HandleConnection the way the dashboard viewer does: it reads messages continuously
// and sends input, so tests can wait for the events an action should produce.
type viewerSession struct {
	t      *testing.T
	conn   net.Conn
	events chan Event
	done   chan error
}

func startViewerSession(t *testing.T, targetURL string) *viewerSession {
	t.Helper()
	return startViewerSessionWithLogger(t, targetURL, nil)
}

func startProxyWithAuth(t *testing.T, targetURL string, auth Auth, logger session.SessionLogger) *viewerSession {
	t.Helper()
	return startViewerSessionWith(t, WebAppProxyConfig{TargetURL: targetURL, SessionID: "test-session", SessionLogger: logger, Auth: auth})
}

func startViewerSessionWithLogger(t *testing.T, targetURL string, logger session.SessionLogger) *viewerSession {
	t.Helper()
	return startViewerSessionWith(t, WebAppProxyConfig{TargetURL: targetURL, SessionID: "test-session", SessionLogger: logger})
}

func startViewerSessionWith(t *testing.T, config WebAppProxyConfig) *viewerSession {
	t.Helper()
	viewer, gateway := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	s := &viewerSession{t: t, conn: viewer, events: make(chan Event, 64), done: make(chan error, 1)}
	proxy := NewWebAppProxy(config)
	go func() { s.done <- proxy.HandleConnection(ctx, gateway) }()
	go func() {
		for {
			msg, err := readMessage(viewer)
			if err != nil {
				close(s.events)
				return
			}
			if msg.Type == MessageTypeEvent {
				var event Event
				if json.Unmarshal(msg.Payload, &event) == nil {
					s.events <- event
				}
			}
		}
	}()
	t.Cleanup(func() { viewer.Close() })
	return s
}

func (s *viewerSession) send(input map[string]any) {
	s.t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := writeMessage(s.conn, MessageTypeInput, payload); err != nil {
		s.t.Fatalf("send input: %v", err)
	}
}

func (s *viewerSession) click(x, y float64) {
	s.send(map[string]any{"t": "mouse", "kind": "down", "x": x, "y": y, "button": "left", "clickCount": 1})
	s.send(map[string]any{"t": "mouse", "kind": "up", "x": x, "y": y, "button": "left", "clickCount": 1})
}

func (s *viewerSession) waitFor(description string, match func(Event) bool) Event {
	s.t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case event, ok := <-s.events:
			if !ok {
				s.t.Fatalf("connection closed while waiting for %s", description)
			}
			if match(event) {
				return event
			}
		case <-timeout:
			s.t.Fatalf("timed out waiting for %s", description)
		}
	}
}

func TestHandleConnectionDispatchesInput(t *testing.T) {
	if !IsSupported() {
		t.Skip("no browser installed")
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("other origin"))
	}))
	defer other.Close()

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/clicked":
			_, _ = w.Write([]byte(`<html><body>clicked</body></html>`))
		case "/search":
			_, _ = w.Write([]byte(`<html><body>results for ` + r.URL.Query().Get("q") + `</body></html>`))
		default:
			_, _ = w.Write([]byte(`<html><body style="margin:0">
<a href="/clicked" style="position:absolute;left:0;top:0;width:200px;height:100px;display:block">go</a>
<form action="/search" style="position:absolute;left:0;top:200px;margin:0">
  <input name="q" style="width:300px;height:40px;box-sizing:border-box">
</form>
<a href="` + other.URL + `/x" style="position:absolute;left:0;top:400px;width:200px;height:100px;display:block">ext</a>
</body></html>`))
		}
	}))
	defer app.Close()

	s := startViewerSession(t, app.URL)
	navigatedTo := func(path string) func(Event) bool {
		return func(e Event) bool { return e.Kind == EventKindNavigated && e.URL == app.URL+path }
	}

	s.waitFor("the page to load", navigatedTo("/"))

	s.click(50, 50)
	s.waitFor("the link click to navigate", navigatedTo("/clicked"))

	s.send(map[string]any{"t": "nav", "action": "back"})
	s.waitFor("back to return to the page", navigatedTo("/"))

	s.click(100, 220)
	for _, ch := range "hi" {
		s.send(map[string]any{"t": "key", "kind": "down", "key": string(ch), "code": "Key" + strings.ToUpper(string(ch)), "text": string(ch)})
		s.send(map[string]any{"t": "key", "kind": "up", "key": string(ch), "code": "Key" + strings.ToUpper(string(ch))})
	}
	s.send(map[string]any{"t": "key", "kind": "down", "key": "Enter", "code": "Enter", "text": "\r"})
	s.send(map[string]any{"t": "key", "kind": "up", "key": "Enter", "code": "Enter"})
	s.waitFor("typing and Enter to submit the form", navigatedTo("/search?q=hi"))

	s.send(map[string]any{"t": "nav", "action": "back"})
	s.waitFor("back to return to the page", navigatedTo("/"))

	s.click(50, 450)
	blocked := s.waitFor("the other-origin link to be blocked", func(e Event) bool { return e.Kind == EventKindBlocked })
	if !strings.HasPrefix(blocked.URL, other.URL) {
		t.Fatalf("blocked %q, want a request to %s", blocked.URL, other.URL)
	}
}

func TestDispatchRejectsUnknownInput(t *testing.T) {
	d := &inputDispatcher{width: defaultViewportWidth, height: defaultViewportHeight}
	for _, payload := range []string{
		`{"t":"eval","expression":"document.cookie"}`,
		`{"t":"mouse","kind":"teleport","x":1,"y":1,"button":"left"}`,
		`{"t":"mouse","kind":"down","x":1,"y":1,"button":"left","clickCount":9}`,
		`{"t":"key","kind":"down","key":"a","text":"this text is far too long"}`,
		`{"t":"nav","action":"navigate"}`,
		`not json`,
	} {
		if err := d.dispatch(context.Background(), []byte(payload)); err == nil {
			t.Errorf("%s: expected an error", payload)
		}
	}
}
