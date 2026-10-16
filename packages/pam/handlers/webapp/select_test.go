package webapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSelectMenuIsDrawnByTheViewer(t *testing.T) {
	if !IsSupported() {
		t.Skip("no browser installed")
	}

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body style="margin:0">
<select name="priority" onchange="location.search = '?p=' + this.value"
        style="position:absolute;left:0;top:0;width:200px;height:40px">
  <option>low</option><option selected>normal</option><option>high</option>
</select></body></html>`))
	}))
	defer app.Close()

	s := startViewerSession(t, app.URL)
	s.waitFor("the page to load", func(e Event) bool { return e.Kind == EventKindNavigated && e.URL == app.URL+"/" })

	s.click(100, 20)
	menu := s.waitFor("the select menu", func(e Event) bool { return e.Kind == EventKindSelect })
	var labels []string
	for _, o := range menu.Options {
		labels = append(labels, o.Label)
	}
	if strings.Join(labels, ",") != "low,normal,high" || menu.Selected == nil || *menu.Selected != 1 {
		t.Fatalf("menu = %v selected %v, want low,normal,high with normal selected", labels, menu.Selected)
	}
	if menu.Rect == nil || menu.Rect.W != 200 || menu.Rect.H != 40 {
		t.Fatalf("menu rect = %+v, want the select's 200x40 box", menu.Rect)
	}

	s.send(map[string]any{"t": "select", "index": 2})
	s.waitFor("choosing high to fire the page's change handler", func(e Event) bool {
		return e.Kind == EventKindNavigated && e.URL == app.URL+"/?p=high"
	})
}
