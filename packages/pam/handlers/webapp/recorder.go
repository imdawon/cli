package webapp

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/Infisical/infisical-merge/packages/pam/session"
	"github.com/rs/zerolog/log"
)

const (
	// The screencast sends a frame on every visual change; an animated page would otherwise record several
	// per second. The newest frame inside a window is kept and written when the window closes.
	recordFrameInterval = 500 * time.Millisecond
	recordMoveInterval  = 100 * time.Millisecond

	maskedValue = "[MASKED]"
)

// Action envelopes recorded alongside frames, ordered by the session logger's elapsed time.
type recordedAction struct {
	Kind   string  `json:"kind"`
	Action string  `json:"action,omitempty"`
	X      float64 `json:"x,omitempty"`
	Y      float64 `json:"y,omitempty"`
	Button string  `json:"button,omitempty"`
	Target string  `json:"target,omitempty"`
	Key    string  `json:"key,omitempty"`
	URL    string  `json:"url,omitempty"`
	Method string  `json:"method,omitempty"`
	Status int     `json:"status,omitempty"`
	Error  string  `json:"error,omitempty"`
	Width  int     `json:"w,omitempty"`
	Height int     `json:"h,omitempty"`
}

// recorder writes what the viewer saw and did into the session recording. It runs at the gateway, so the
// viewer has no way to alter what is captured. Request bodies and headers are never recorded.
type recorder struct {
	logger session.SessionLogger
	mask   func(string) string

	mu             sync.Mutex
	lastFrameAt    time.Time
	pendingFrame   []byte
	forceNextFrame bool
	lastMoveAt     time.Time
}

func newRecorder(logger session.SessionLogger, mask func(string) string) *recorder {
	if mask == nil {
		mask = func(s string) string { return s }
	}
	return &recorder{logger: logger, mask: mask}
}

func (r *recorder) write(eventType session.SessionEventType, data []byte) {
	if r.logger == nil {
		return
	}
	if err := r.logger.LogSessionEvent(session.SessionEvent{
		Timestamp:   time.Now(),
		EventType:   eventType,
		ChannelType: session.SessionChannelWebApp,
		Data:        data,
	}); err != nil {
		log.Warn().Err(err).Msg("Failed to record web app event")
	}
}

func (r *recorder) frame(jpeg []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.forceNextFrame || now.Sub(r.lastFrameAt) >= recordFrameInterval {
		r.forceNextFrame = false
		r.pendingFrame = nil
		r.lastFrameAt = now
		r.write(session.SessionEventWebAppFrame, jpeg)
		return
	}
	r.pendingFrame = jpeg
}

// flush writes the newest held-back frame once its window has passed, so the page's final state after a
// burst of changes is recorded even if nothing changes afterwards.
func (r *recorder) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingFrame == nil || time.Since(r.lastFrameAt) < recordFrameInterval {
		return
	}
	frame := r.pendingFrame
	r.pendingFrame = nil
	r.lastFrameAt = time.Now()
	r.write(session.SessionEventWebAppFrame, frame)
}

func (r *recorder) action(a recordedAction) {
	a.URL = r.mask(a.URL)
	a.Target = r.mask(a.Target)
	data, err := json.Marshal(a)
	if err != nil {
		return
	}
	r.write(session.SessionEventWebAppAction, data)
}

// input records a click, key or scroll and makes sure the frame showing its result is kept.
func (r *recorder) input(a recordedAction) {
	r.mu.Lock()
	if a.Kind == "mouse" && a.Action == "move" {
		if time.Since(r.lastMoveAt) < recordMoveInterval {
			r.mu.Unlock()
			return
		}
		r.lastMoveAt = time.Now()
	} else {
		r.forceNextFrame = true
	}
	r.mu.Unlock()
	r.action(a)
}
