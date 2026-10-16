package webapp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Infisical/infisical-merge/packages/pam/session"
)

type fakeSessionLogger struct {
	mu     sync.Mutex
	events []session.SessionEvent
}

func (f *fakeSessionLogger) LogEntry(session.SessionLogEntry) error { return nil }
func (f *fakeSessionLogger) LogHttpEvent(session.HttpEvent) error   { return nil }
func (f *fakeSessionLogger) Close() error                           { return nil }
func (f *fakeSessionLogger) LogSessionEvent(e session.SessionEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakeSessionLogger) snapshot() (frames int, actions []recordedAction, raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		raw = append(raw, e.Data...)
		switch e.EventType {
		case session.SessionEventWebAppFrame:
			frames++
		case session.SessionEventWebAppAction:
			var a recordedAction
			if json.Unmarshal(e.Data, &a) == nil {
				actions = append(actions, a)
			}
		}
	}
	return frames, actions, raw
}

func TestRecorderThrottlesFramesButKeepsTheLast(t *testing.T) {
	logger := &fakeSessionLogger{}
	rec := newRecorder(logger)

	for i := 0; i < 10; i++ {
		rec.frame([]byte{0xFF, 0xD8, byte(i)})
	}
	if frames, _, _ := logger.snapshot(); frames != 1 {
		t.Fatalf("recorded %d frames from a burst, want 1", frames)
	}

	time.Sleep(recordFrameInterval)
	rec.flush()
	frames, _, raw := logger.snapshot()
	if frames != 2 || raw[len(raw)-1] != 9 {
		t.Fatalf("flush recorded %d frames ending in %d, want the burst's last frame", frames, raw[len(raw)-1])
	}

	rec.input(recordedAction{Kind: "mouse", Action: "down"})
	rec.frame([]byte{0xFF, 0xD8, 42})
	if frames, _, _ := logger.snapshot(); frames != 3 {
		t.Fatal("the frame after a click should be recorded immediately")
	}
}

func TestRecordingCapturesActionsWithoutSecrets(t *testing.T) {
	if !IsSupported() {
		t.Skip("no browser installed")
	}

	const password = "zq7Vx9Lk"
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/login":
			http.Redirect(w, r, "/done", http.StatusSeeOther)
		case "/done":
			_, _ = w.Write([]byte(`<html><body>signed in</body></html>`))
		default:
			_, _ = w.Write([]byte(`<html><body style="margin:0">
<form method="POST" action="/login" style="margin:0">
  <input name="username" placeholder="Username" style="position:absolute;left:0;top:0;width:300px;height:40px">
  <input name="password" type="password" placeholder="Password" style="position:absolute;left:0;top:100px;width:300px;height:40px">
  <button type="submit" style="position:absolute;left:0;top:200px;width:200px;height:40px">Sign in</button>
</form>
<a href="https://blocked.example/x" style="position:absolute;left:0;top:300px;width:200px;height:40px;display:block">ext</a>
</body></html>`))
		}
	}))
	defer app.Close()

	logger := &fakeSessionLogger{}
	s := startViewerSessionWithLogger(t, app.URL, logger)
	navigatedTo := func(path string) func(Event) bool {
		return func(e Event) bool { return e.Kind == EventKindNavigated && e.URL == app.URL+path }
	}
	typeText := func(text string) {
		for _, ch := range text {
			s.send(map[string]any{"t": "key", "kind": "down", "key": string(ch), "code": "", "text": string(ch)})
			s.send(map[string]any{"t": "key", "kind": "up", "key": string(ch), "code": ""})
		}
	}

	s.waitFor("the page to load", navigatedTo("/"))
	s.click(50, 20)
	typeText("bob")
	s.click(50, 120)
	typeText(password)
	s.click(50, 220)
	s.waitFor("the form to submit", navigatedTo("/done"))
	s.send(map[string]any{"t": "nav", "action": "back"})
	s.waitFor("back to the form", navigatedTo("/"))
	s.click(50, 320)
	s.waitFor("the external link to be blocked", func(e Event) bool { return e.Kind == EventKindBlocked })
	time.Sleep(2 * recorderFlushInterval)

	frames, actions, raw := logger.snapshot()
	if frames == 0 {
		t.Fatal("no frames recorded")
	}
	if bytes.Contains(raw, []byte(password)) {
		t.Fatal("the typed password reached the recording")
	}

	var keys []string
	var sawPost, sawBlocked, sawSignInClick, sawNavigate bool
	for _, a := range actions {
		switch a.Kind {
		case "key":
			keys = append(keys, a.Key)
		case "request":
			if a.Method == "POST" && strings.HasSuffix(a.URL, "/login") && a.Status == http.StatusSeeOther {
				sawPost = true
			}
		case "blocked":
			sawBlocked = strings.HasPrefix(a.URL, "https://blocked.example")
		case "mouse":
			if a.Action == "down" && a.Target == `button "Sign in"` {
				sawSignInClick = true
			}
		case "navigate":
			if a.URL == app.URL+"/done" {
				sawNavigate = true
			}
		}
	}

	wantKeys := append([]string{"b", "o", "b"}, strings.Split(strings.Repeat(maskedValue+",", len(password)-1)+maskedValue, ",")...)
	if strings.Join(keys, ",") != strings.Join(wantKeys, ",") {
		t.Errorf("recorded keys %v, want %v", keys, wantKeys)
	}
	if !sawPost {
		t.Error("the login POST and its 303 status weren't recorded")
	}
	if !sawBlocked {
		t.Error("the blocked request wasn't recorded")
	}
	if !sawSignInClick {
		t.Error(`the click wasn't labelled button "Sign in"`)
	}
	if !sawNavigate {
		t.Error("the navigation to /done wasn't recorded")
	}
}
