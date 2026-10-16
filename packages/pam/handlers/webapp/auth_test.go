package webapp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInjectedHeaderReplacesThePagesOwn(t *testing.T) {
	auth := Auth{Method: "header", HeaderName: "X-Acme-Token", HeaderValue: "real"}
	headers, ok := auth.withInjectedHeader(map[string]string{"x-acme-token": "forged", "Accept": "text/html"})
	if !ok {
		t.Fatal("expected a header to inject")
	}
	var got []string
	for _, h := range headers {
		got = append(got, h.Name+"="+h.Value)
	}
	if strings.Join(got, ",") != "Accept=text/html,X-Acme-Token=real" {
		t.Fatalf("headers = %v", got)
	}

	if _, ok := (Auth{Method: "none"}).withInjectedHeader(nil); ok {
		t.Fatal("no header should be injected for the none sign-in method")
	}
	basic, _ := (Auth{Method: "basic", Username: "admin", Password: "pw"}).withInjectedHeader(nil)
	if basic[0].Name != "Authorization" || basic[0].Value != "Basic YWRtaW46cHc=" {
		t.Fatalf("basic auth header = %+v", basic[0])
	}
}

// An app that refuses any request without the right credential, assets included.
func credentialProtectedApp(t *testing.T, check func(*http.Request) bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !check(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/app.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte(`document.title = "loaded";`))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>dashboard<script src="/app.js"></script></body></html>`))
		}
	}))
}

func statusesFor(t *testing.T, appURL string, auth Auth) (map[string]int, []byte) {
	t.Helper()
	logger := &fakeSessionLogger{}
	viewer := startProxyWithAuth(t, appURL, auth, logger)
	viewer.waitFor("the page to load", func(e Event) bool { return e.Kind == EventKindNavigated })
	// The page's script request finishes after the navigation event
	time.Sleep(time.Second)

	statuses := map[string]int{}
	_, actions, raw := logger.snapshot()
	for _, a := range actions {
		if a.Kind == "request" {
			statuses[strings.TrimPrefix(a.URL, appURL)] = a.Status
		}
	}
	return statuses, raw
}

func TestGatewayInjectsCredentialsTheViewerNeverHas(t *testing.T) {
	if !IsSupported() {
		t.Skip("no browser installed")
	}

	const token = "acme-header-secret-7f3"
	app := credentialProtectedApp(t, func(r *http.Request) bool { return r.Header.Get("X-Acme-Token") == token })
	defer app.Close()

	without, _ := statusesFor(t, app.URL, Auth{Method: "none"})
	if without["/"] != http.StatusUnauthorized {
		t.Fatalf("without a credential the app returned %d, want 401", without["/"])
	}

	with, raw := statusesFor(t, app.URL, Auth{Method: "header", HeaderName: "X-Acme-Token", HeaderValue: token})
	if with["/"] != http.StatusOK || with["/app.js"] != http.StatusOK {
		t.Fatalf("with the header credential got %v, want 200 for the page and its script", with)
	}
	if bytes.Contains(raw, []byte(token)) {
		t.Fatal("the injected credential reached the recording")
	}

	basicApp := credentialProtectedApp(t, func(r *http.Request) bool {
		user, pass, ok := r.BasicAuth()
		return ok && user == "admin" && pass == "hunter2"
	})
	defer basicApp.Close()
	basic, _ := statusesFor(t, basicApp.URL, Auth{Method: "basic", Username: "admin", Password: "hunter2"})
	if basic["/"] != http.StatusOK {
		t.Fatalf("with basic auth the app returned %d, want 200", basic["/"])
	}
}
