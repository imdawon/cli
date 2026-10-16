package webapp

import (
	"encoding/base64"
	"sort"
	"strings"
)

// Auth is how the gateway signs the browser in to the web app. The credential is added to requests at the
// gateway, so it never reaches the viewer, the page's scripts, or the recording.
type Auth struct {
	Method      string // "none", "basic" or "header"
	Username    string
	Password    string
	HeaderName  string
	HeaderValue string
}

// header returns the header to inject, or ok=false when the account signs in with nothing.
func (a Auth) header() (name, value string, ok bool) {
	switch a.Method {
	case "basic":
		credential := base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))
		return "Authorization", "Basic " + credential, true
	case "header":
		if a.HeaderName == "" {
			return "", "", false
		}
		return a.HeaderName, a.HeaderValue, true
	default:
		return "", "", false
	}
}

type headerEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// withInjectedHeader rebuilds a paused request's headers with the credential added. CDP replaces the whole
// header set on continue, so the originals are carried over, minus any the page sent under the same name.
func (a Auth) withInjectedHeader(original map[string]string) ([]headerEntry, bool) {
	name, value, ok := a.header()
	if !ok {
		return nil, false
	}
	headers := make([]headerEntry, 0, len(original)+1)
	for k, v := range original {
		if strings.EqualFold(k, name) {
			continue
		}
		headers = append(headers, headerEntry{Name: k, Value: v})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].Name < headers[j].Name })
	return append(headers, headerEntry{Name: name, Value: value}), true
}
