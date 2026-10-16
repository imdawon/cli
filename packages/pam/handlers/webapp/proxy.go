package webapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Infisical/infisical-merge/packages/pam/session"
	"github.com/rs/zerolog/log"
)

const (
	defaultViewportWidth  = 1280
	defaultViewportHeight = 800
	screencastJPEGQuality = 70
	recorderFlushInterval = 250 * time.Millisecond
	// Bounds the requests awaiting a response, so a page that never finishes requests can't grow it forever
	maxTrackedRequests = 1000
)

type WebAppProxyConfig struct {
	TargetURL     string
	SessionID     string
	SessionLogger session.SessionLogger
	Auth          Auth
}

// WebAppProxy runs one isolated browser for a session. The gateway is its only CDP client: the viewer
// receives rendered frames and sends typed input messages that the gateway translates into CDP.
type WebAppProxy struct {
	config WebAppProxyConfig
}

func NewWebAppProxy(config WebAppProxyConfig) *WebAppProxy {
	return &WebAppProxy{config: config}
}

// sameOrigin reports whether a request may leave the browser: only the account's own origin is reachable,
// so a page can't be used to reach other internal hosts.
func sameOrigin(target *url.URL, raw string) bool {
	if strings.HasPrefix(raw, "data:") || strings.HasPrefix(raw, "blob:") || raw == "about:blank" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return parsed.Scheme == target.Scheme && parsed.Host == target.Host
}

// isViewerClosed reports whether err only means the viewer went away, which ends a session normally.
func isViewerClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

func validateTargetURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid web app URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("web app URL must use http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("web app URL has no host")
	}
	return parsed, nil
}

// HandleConnection serves one viewer connection until the viewer disconnects, the browser exits, or ctx is
// cancelled (session expiry or termination).
func (p *WebAppProxy) HandleConnection(ctx context.Context, clientConn net.Conn) error {
	defer clientConn.Close()

	target, err := validateTargetURL(p.config.TargetURL)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	b, err := launchBrowser(defaultViewportWidth, defaultViewportHeight)
	if err != nil {
		return err
	}
	defer b.close()

	logger := log.With().Str("sessionId", p.config.SessionID).Logger()

	var writeMu sync.Mutex
	send := func(msgType MessageType, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeMessage(clientConn, msgType, payload)
	}
	sendEvent := func(event Event) error {
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		return send(MessageTypeEvent, data)
	}

	pageSession, err := openPage(ctx, b.cdp, target.String())
	if err != nil {
		return err
	}

	if err := sendEvent(Event{Kind: EventKindReady, Width: defaultViewportWidth, Height: defaultViewportHeight}); err != nil {
		return err
	}

	// Input is dispatched in order from this one goroutine. A malformed message is dropped, not fatal, so a
	// buggy client can't end a session it's still using.
	rec := newRecorder(p.config.SessionLogger)
	dispatcher := &inputDispatcher{
		cdp: b.cdp, pageSession: pageSession, width: defaultViewportWidth, height: defaultViewportHeight, recorder: rec,
		notify: sendEvent,
	}
	viewerGone := make(chan error, 1)
	go func() {
		for {
			msg, err := readMessage(clientConn)
			if err != nil {
				viewerGone <- err
				return
			}
			if msg.Type != MessageTypeInput {
				logger.Debug().Uint8("type", uint8(msg.Type)).Msg("Dropped unexpected web app viewer message")
				continue
			}
			if err := dispatcher.dispatch(ctx, msg.Payload); err != nil {
				if ctx.Err() != nil {
					return
				}
				logger.Debug().Err(err).Msg("Dropped web app input")
			}
		}
	}()

	type pendingRequest struct{ method, url string }
	requests := make(map[string]pendingRequest)
	recordRequest := func(method, rawURL string, status int, errorText string) {
		if strings.HasPrefix(rawURL, "data:") || strings.HasPrefix(rawURL, "blob:") {
			return
		}
		rec.action(recordedAction{Kind: "request", Method: method, URL: rawURL, Status: status, Error: errorText})
	}

	flushTicker := time.NewTicker(recorderFlushInterval)
	defer flushTicker.Stop()

	var seq uint64
	for {
		select {
		case <-flushTicker.C:
			rec.flush()
		case <-ctx.Done():
			_ = sendEvent(Event{Kind: EventKindEnded, Reason: "session ended"})
			return nil
		case err := <-viewerGone:
			if isViewerClosed(err) {
				logger.Info().Msg("Web app viewer disconnected")
				return nil
			}
			return fmt.Errorf("viewer connection: %w", err)
		case <-b.exited:
			_ = sendEvent(Event{Kind: EventKindEnded, Reason: "browser exited"})
			return errors.New("web app browser exited unexpectedly")
		case event := <-b.cdp.events:
			if event.SessionID != pageSession {
				continue
			}
			switch event.Method {
			case "Network.requestWillBeSent":
				var params struct {
					RequestID string `json:"requestId"`
					Request   struct {
						Method string `json:"method"`
						URL    string `json:"url"`
					} `json:"request"`
					RedirectResponse *struct {
						Status int `json:"status"`
					} `json:"redirectResponse"`
				}
				if err := json.Unmarshal(event.Params, &params); err != nil {
					continue
				}
				// A redirect reuses the request id, so the hop that just finished is recorded with its status
				if prev, ok := requests[params.RequestID]; ok && params.RedirectResponse != nil {
					recordRequest(prev.method, prev.url, params.RedirectResponse.Status, "")
				}
				if len(requests) < maxTrackedRequests {
					requests[params.RequestID] = pendingRequest{method: params.Request.Method, url: params.Request.URL}
				}
			case "Network.responseReceived":
				var params struct {
					RequestID string `json:"requestId"`
					Response  struct {
						Status int `json:"status"`
					} `json:"response"`
				}
				if err := json.Unmarshal(event.Params, &params); err != nil {
					continue
				}
				if req, ok := requests[params.RequestID]; ok {
					recordRequest(req.method, req.url, params.Response.Status, "")
					delete(requests, params.RequestID)
				}
			case "Network.loadingFailed":
				var params struct {
					RequestID string `json:"requestId"`
					ErrorText string `json:"errorText"`
				}
				if err := json.Unmarshal(event.Params, &params); err != nil {
					continue
				}
				if req, ok := requests[params.RequestID]; ok {
					recordRequest(req.method, req.url, 0, params.ErrorText)
					delete(requests, params.RequestID)
				}
			case "Fetch.requestPaused":
				var params struct {
					RequestID string `json:"requestId"`
					Request   struct {
						URL     string            `json:"url"`
						Headers map[string]string `json:"headers"`
					} `json:"request"`
				}
				if err := json.Unmarshal(event.Params, &params); err != nil {
					continue
				}
				if sameOrigin(target, params.Request.URL) {
					continueParams := map[string]any{"requestId": params.RequestID}
					if headers, ok := p.config.Auth.withInjectedHeader(params.Request.Headers); ok {
						continueParams["headers"] = headers
					}
					_ = b.cdp.notify(pageSession, "Fetch.continueRequest", continueParams)
					continue
				}
				_ = b.cdp.notify(pageSession, "Fetch.failRequest", map[string]any{"requestId": params.RequestID, "errorReason": "BlockedByClient"})
				logger.Info().Str("url", params.Request.URL).Msg("Blocked web app request to another origin")
				rec.action(recordedAction{Kind: "blocked", URL: params.Request.URL})
				if err := sendEvent(Event{Kind: EventKindBlocked, URL: params.Request.URL}); err != nil {
					if isViewerClosed(err) {
						return nil
					}
					return err
				}
			case "Page.screencastFrame":
				if err := forwardFrame(b.cdp, pageSession, event.Params, &seq, send, rec); err != nil {
					if isViewerClosed(err) {
						logger.Info().Msg("Web app viewer disconnected")
						return nil
					}
					return err
				}
			case "Page.frameNavigated":
				var params struct {
					Frame struct {
						ParentID string `json:"parentId"`
						URL      string `json:"url"`
					} `json:"frame"`
				}
				if err := json.Unmarshal(event.Params, &params); err == nil && params.Frame.ParentID == "" {
					rec.action(recordedAction{Kind: "navigate", URL: params.Frame.URL})
					if err := sendEvent(Event{Kind: EventKindNavigated, URL: params.Frame.URL}); err != nil {
						if isViewerClosed(err) {
							return nil
						}
						return err
					}
				}
			}
		}
	}
}

// openPage creates the page target, attaches to it, and starts the screencast. It returns the CDP session
// id that page events and commands are scoped to.
func openPage(ctx context.Context, cdp *cdpClient, targetURL string) (string, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := cdp.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
		return "", err
	}

	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := cdp.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &attached); err != nil {
		return "", err
	}
	pageSession := attached.SessionID

	steps := []struct {
		method string
		params any
	}{
		{"Page.enable", nil},
		{"Network.enable", nil},
		{"Emulation.setDeviceMetricsOverride", map[string]any{
			"width": defaultViewportWidth, "height": defaultViewportHeight, "deviceScaleFactor": 1, "mobile": false,
		}},
		{"Page.startScreencast", map[string]any{
			"format": "jpeg", "quality": screencastJPEGQuality,
			"maxWidth": defaultViewportWidth, "maxHeight": defaultViewportHeight,
		}},
		// Every request pauses here so the handler can refuse any origin but the account's
		{"Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*"}}}},
	}
	for _, step := range steps {
		if err := cdp.call(ctx, pageSession, step.method, step.params, nil); err != nil {
			return "", err
		}
	}
	// Navigate last: the first navigation away from about:blank swaps the page into a new renderer, and
	// starting the screencast during that swap fails with "Not attached to an active page". It isn't awaited:
	// its request pauses in Fetch until the event loop, which starts after this returns, lets it through.
	if err := cdp.notify(pageSession, "Page.navigate", map[string]any{"url": targetURL}); err != nil {
		return "", err
	}
	return pageSession, nil
}

func forwardFrame(cdp *cdpClient, pageSession string, raw json.RawMessage, seq *uint64, send func(MessageType, []byte) error, rec *recorder) error {
	var frame struct {
		Data      string `json:"data"`
		SessionID int    `json:"sessionId"`
		Metadata  struct {
			DeviceWidth  float64 `json:"deviceWidth"`
			DeviceHeight float64 `json:"deviceHeight"`
			Timestamp    float64 `json:"timestamp"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return fmt.Errorf("invalid screencast frame: %w", err)
	}

	// Chrome won't send the next frame until this one is acked. Acking after the write makes the viewer's
	// connection the flow control: a slow link gets fewer frames rather than a growing backlog.
	defer func() {
		_ = cdp.notify(pageSession, "Page.screencastFrameAck", map[string]any{"sessionId": frame.SessionID})
	}()

	jpeg, err := base64.StdEncoding.DecodeString(frame.Data)
	if err != nil {
		return fmt.Errorf("invalid screencast frame data: %w", err)
	}

	rec.frame(jpeg)

	*seq++
	payload, err := encodeFrame(FrameMeta{
		Seq:    *seq,
		Ts:     frame.Metadata.Timestamp,
		Width:  int(frame.Metadata.DeviceWidth),
		Height: int(frame.Metadata.DeviceHeight),
	}, jpeg)
	if err != nil {
		return err
	}
	return send(MessageTypeFrame, payload)
}
