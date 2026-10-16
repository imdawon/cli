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
	"sync"
	"syscall"

	"github.com/rs/zerolog/log"
)

const (
	defaultViewportWidth  = 1280
	defaultViewportHeight = 800
	screencastJPEGQuality = 70
)

type WebAppProxyConfig struct {
	TargetURL string
	SessionID string
}

// WebAppProxy runs one isolated browser for a session. The gateway is its only CDP client: the viewer
// receives rendered frames and, from M2, sends typed input messages that the gateway translates into CDP.
type WebAppProxy struct {
	config WebAppProxyConfig
}

func NewWebAppProxy(config WebAppProxyConfig) *WebAppProxy {
	return &WebAppProxy{config: config}
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

	pageSession, err := p.openPage(ctx, b.cdp, target.String())
	if err != nil {
		return err
	}

	if err := sendEvent(Event{Kind: EventKindReady, Width: defaultViewportWidth, Height: defaultViewportHeight}); err != nil {
		return err
	}

	// The viewer side: M1 only watches for disconnect, and drops whatever it sends.
	viewerGone := make(chan error, 1)
	go func() {
		for {
			if _, err := readMessage(clientConn); err != nil {
				viewerGone <- err
				return
			}
		}
	}()

	var seq uint64
	for {
		select {
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
			case "Page.screencastFrame":
				if err := p.forwardFrame(b.cdp, pageSession, event.Params, &seq, send); err != nil {
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
func (p *WebAppProxy) openPage(ctx context.Context, cdp *cdpClient, targetURL string) (string, error) {
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
	session := attached.SessionID

	steps := []struct {
		method string
		params any
	}{
		{"Page.enable", nil},
		{"Emulation.setDeviceMetricsOverride", map[string]any{
			"width": defaultViewportWidth, "height": defaultViewportHeight, "deviceScaleFactor": 1, "mobile": false,
		}},
		{"Page.startScreencast", map[string]any{
			"format": "jpeg", "quality": screencastJPEGQuality,
			"maxWidth": defaultViewportWidth, "maxHeight": defaultViewportHeight,
		}},
		// Navigate last: the first navigation away from about:blank swaps the page into a new renderer, and
		// starting the screencast during that swap fails with "Not attached to an active page".
		{"Page.navigate", map[string]any{"url": targetURL}},
	}
	for _, step := range steps {
		if err := cdp.call(ctx, session, step.method, step.params, nil); err != nil {
			return "", err
		}
	}
	return session, nil
}

func (p *WebAppProxy) forwardFrame(cdp *cdpClient, pageSession string, raw json.RawMessage, seq *uint64, send func(MessageType, []byte) error) error {
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
