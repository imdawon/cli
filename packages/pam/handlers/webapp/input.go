package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Input arrives from the viewer, which is untrusted. Only these message shapes exist: anything else is
// rejected before it reaches CDP, so the viewer can click, type, scroll, resize and move through history,
// and nothing more.

type inputMessage struct {
	T string `json:"t"`

	// mouse, wheel
	Kind       string  `json:"kind"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Button     string  `json:"button"`
	ClickCount int     `json:"clickCount"`
	DX         float64 `json:"dx"`
	DY         float64 `json:"dy"`

	// key
	Key       string `json:"key"`
	Code      string `json:"code"`
	Text      string `json:"text"`
	Modifiers int    `json:"modifiers"`

	// resize
	W int `json:"w"`
	H int `json:"h"`

	// nav
	Action string `json:"action"`
}

const (
	minViewportWidth  = 320
	minViewportHeight = 240
	maxViewportWidth  = 3840
	maxViewportHeight = 2160

	maxKeyFieldLength = 32
	maxKeyTextLength  = 8
	maxWheelDelta     = 10000

	// CDP modifier bitmask: Alt=1, Ctrl=2, Meta=4, Shift=8
	maxModifiers = 15
)

var mouseEventTypes = map[string]string{"down": "mousePressed", "up": "mouseReleased", "move": "mouseMoved"}
var mouseButtons = map[string]bool{"none": true, "left": true, "middle": true, "right": true}

// Windows virtual key codes for keys that produce no text. Chrome needs them to perform the key's action
// (Backspace deleting, Enter submitting, arrows moving the caret).
var virtualKeyCodes = map[string]int{
	"Backspace": 8, "Tab": 9, "Enter": 13, "Shift": 16, "Control": 17, "Alt": 18, "Escape": 27,
	"PageUp": 33, "PageDown": 34, "End": 35, "Home": 36,
	"ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40, "Delete": 46, "Meta": 91,
}

func virtualKeyCode(key, code string) int {
	if vk, ok := virtualKeyCodes[key]; ok {
		return vk
	}
	switch {
	case strings.HasPrefix(code, "Key") && len(code) == 4:
		return int(code[3])
	case strings.HasPrefix(code, "Digit") && len(code) == 6:
		return int(code[5])
	case code == "Space":
		return 32
	}
	if utf8.RuneCountInString(key) == 1 {
		r, _ := utf8.DecodeRuneInString(key)
		if r >= 'a' && r <= 'z' {
			return int(r - 32)
		}
		if r < 128 {
			return int(r)
		}
	}
	return 0
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// inputDispatcher translates viewer input into CDP commands for one page. It's used from a single goroutine.
type inputDispatcher struct {
	cdp         *cdpClient
	pageSession string
	width       int
	height      int
}

func (d *inputDispatcher) dispatch(ctx context.Context, payload []byte) error {
	var msg inputMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return fmt.Errorf("invalid input message: %w", err)
	}

	switch msg.T {
	case "mouse":
		eventType, ok := mouseEventTypes[msg.Kind]
		if !ok || !mouseButtons[msg.Button] || msg.ClickCount < 0 || msg.ClickCount > 3 ||
			msg.Modifiers < 0 || msg.Modifiers > maxModifiers {
			return errors.New("invalid mouse input")
		}
		return d.cdp.call(ctx, d.pageSession, "Input.dispatchMouseEvent", map[string]any{
			"type":       eventType,
			"x":          clamp(msg.X, 0, float64(d.width-1)),
			"y":          clamp(msg.Y, 0, float64(d.height-1)),
			"button":     msg.Button,
			"clickCount": msg.ClickCount,
			"modifiers":  msg.Modifiers,
		}, nil)

	case "wheel":
		return d.cdp.call(ctx, d.pageSession, "Input.dispatchMouseEvent", map[string]any{
			"type":   "mouseWheel",
			"x":      clamp(msg.X, 0, float64(d.width-1)),
			"y":      clamp(msg.Y, 0, float64(d.height-1)),
			"deltaX": clamp(msg.DX, -maxWheelDelta, maxWheelDelta),
			"deltaY": clamp(msg.DY, -maxWheelDelta, maxWheelDelta),
		}, nil)

	case "key":
		if (msg.Kind != "down" && msg.Kind != "up") || len(msg.Key) > maxKeyFieldLength ||
			len(msg.Code) > maxKeyFieldLength || utf8.RuneCountInString(msg.Text) > maxKeyTextLength ||
			msg.Modifiers < 0 || msg.Modifiers > maxModifiers {
			return errors.New("invalid key input")
		}
		eventType := "keyUp"
		params := map[string]any{
			"key":                   msg.Key,
			"code":                  msg.Code,
			"modifiers":             msg.Modifiers,
			"windowsVirtualKeyCode": virtualKeyCode(msg.Key, msg.Code),
		}
		if msg.Kind == "down" {
			// keyDown with text also types the character; rawKeyDown is a keypress that produces none
			eventType = "rawKeyDown"
			if msg.Text != "" {
				eventType = "keyDown"
				params["text"] = msg.Text
				params["unmodifiedText"] = msg.Text
			}
		}
		params["type"] = eventType
		return d.cdp.call(ctx, d.pageSession, "Input.dispatchKeyEvent", params, nil)

	case "resize":
		width := int(clamp(float64(msg.W), minViewportWidth, maxViewportWidth))
		height := int(clamp(float64(msg.H), minViewportHeight, maxViewportHeight))
		if width == d.width && height == d.height {
			return nil
		}
		if err := d.cdp.call(ctx, d.pageSession, "Emulation.setDeviceMetricsOverride", map[string]any{
			"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false,
		}, nil); err != nil {
			return err
		}
		d.width, d.height = width, height
		// The screencast keeps its original size cap until restarted
		if err := d.cdp.call(ctx, d.pageSession, "Page.stopScreencast", nil, nil); err != nil {
			return err
		}
		return d.cdp.call(ctx, d.pageSession, "Page.startScreencast", map[string]any{
			"format": "jpeg", "quality": screencastJPEGQuality, "maxWidth": width, "maxHeight": height,
		}, nil)

	case "nav":
		switch msg.Action {
		case "reload":
			return d.cdp.call(ctx, d.pageSession, "Page.reload", nil, nil)
		case "back", "forward":
			return d.goHistory(ctx, msg.Action)
		default:
			return errors.New("invalid nav input")
		}

	default:
		return fmt.Errorf("unknown input type %q", msg.T)
	}
}

func (d *inputDispatcher) goHistory(ctx context.Context, direction string) error {
	var history struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if err := d.cdp.call(ctx, d.pageSession, "Page.getNavigationHistory", nil, &history); err != nil {
		return err
	}
	index := history.CurrentIndex - 1
	if direction == "forward" {
		index = history.CurrentIndex + 1
	}
	if index < 0 || index >= len(history.Entries) {
		return nil
	}
	return d.cdp.call(ctx, d.pageSession, "Page.navigateToHistoryEntry", map[string]any{"entryId": history.Entries[index].ID}, nil)
}
