package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
