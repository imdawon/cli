package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
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

	// select: the option picked in the viewer's menu, or -1 to dismiss it
	Index int `json:"index"`
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
	recorder    *recorder
	notify      func(Event) error

	// The <select> whose menu the viewer is showing, and whether the release of the click that opened it
	// is still to come (it isn't sent to Chrome, or Chrome would open its own, invisible menu)
	selectObjectID   string
	swallowMouseUpOn string
}

// Labels what sits under a click for the recording. It never reads a field's value, only a button's caption,
// so a password can't reach the recording through this path.
const describeElementScript = `(() => {
  const at = document.elementFromPoint(%f, %f);
  if (!at) return "";
  const el = at.closest("a,button,input,select,textarea,label,summary,[role]") || at;
  const tag = el.tagName.toLowerCase();
  const type = (el.getAttribute("type") || "").toLowerCase();
  let name = el.getAttribute("aria-label") || "";
  if (!name) {
    if (tag === "input") name = type === "submit" || type === "button" ? el.value : el.placeholder || el.name || "";
    else if (tag === "textarea" || tag === "select") name = el.name || "";
    else name = el.innerText || "";
  }
  name = name.replace(/\s+/g, " ").trim().slice(0, 80);
  const kind = tag === "a" ? "link" : tag === "input" ? (type || "text") + " field" : tag;
  return name ? kind + ' "' + name + '"' : kind;
})()`

const focusedIsPasswordScript = `(() => { const el = document.activeElement; return !!el && el.type === "password"; })()`

func (d *inputDispatcher) evaluate(ctx context.Context, expression string, result any) error {
	var response struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if err := d.cdp.call(ctx, d.pageSession, "Runtime.evaluate", map[string]any{
		"expression": expression, "returnByValue": true,
	}, &response); err != nil {
		return err
	}
	if len(response.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(response.Result.Value, result)
}

func (d *inputDispatcher) record(a recordedAction) {
	if d.recorder != nil {
		d.recorder.input(a)
	}
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
		x, y := clamp(msg.X, 0, float64(d.width-1)), clamp(msg.Y, 0, float64(d.height-1))
		if msg.Kind == "up" && d.swallowMouseUpOn == msg.Button {
			d.swallowMouseUpOn = ""
			return nil
		}
		recorded := recordedAction{Kind: "mouse", Action: msg.Kind, X: x, Y: y, Button: msg.Button}
		if msg.Kind == "down" && d.recorder != nil {
			_ = d.evaluate(ctx, fmt.Sprintf(describeElementScript, x, y), &recorded.Target)
		}
		if msg.Kind == "down" && msg.Button == "left" && d.notify != nil {
			opened, err := d.openSelect(ctx, x, y)
			if err != nil {
				return err
			}
			if opened {
				d.swallowMouseUpOn = msg.Button
				d.record(recorded)
				return nil
			}
		}
		if err := d.cdp.call(ctx, d.pageSession, "Input.dispatchMouseEvent", map[string]any{
			"type":       eventType,
			"x":          x,
			"y":          y,
			"button":     msg.Button,
			"clickCount": msg.ClickCount,
			"modifiers":  msg.Modifiers,
		}, nil); err != nil {
			return err
		}
		d.record(recorded)
		return nil

	case "wheel":
		x, y := clamp(msg.X, 0, float64(d.width-1)), clamp(msg.Y, 0, float64(d.height-1))
		if err := d.cdp.call(ctx, d.pageSession, "Input.dispatchMouseEvent", map[string]any{
			"type":   "mouseWheel",
			"x":      x,
			"y":      y,
			"deltaX": clamp(msg.DX, -maxWheelDelta, maxWheelDelta),
			"deltaY": clamp(msg.DY, -maxWheelDelta, maxWheelDelta),
		}, nil); err != nil {
			return err
		}
		d.record(recordedAction{Kind: "mouse", Action: "wheel", X: x, Y: y})
		return nil

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
		// Decided before dispatch: Enter or Tab can move focus out of the password field
		recordedKey := msg.Key
		if msg.Kind == "down" && d.recorder != nil {
			var isPassword bool
			if err := d.evaluate(ctx, focusedIsPasswordScript, &isPassword); err != nil || isPassword {
				recordedKey = maskedValue
			}
		}
		if err := d.cdp.call(ctx, d.pageSession, "Input.dispatchKeyEvent", params, nil); err != nil {
			return err
		}
		if msg.Kind == "down" {
			d.record(recordedAction{Kind: "key", Key: recordedKey})
		}
		return nil

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
		if err := d.callPage(ctx, "Page.stopScreencast", nil, nil); err != nil {
			return err
		}
		if err := d.callPage(ctx, "Page.startScreencast", map[string]any{
			"format": "jpeg", "quality": screencastJPEGQuality, "maxWidth": width, "maxHeight": height,
		}, nil); err != nil {
			return err
		}
		d.record(recordedAction{Kind: "resize", Width: width, Height: height})
		return nil

	case "select":
		if msg.Index < -1 || msg.Index >= maxSelectOptions {
			return errors.New("invalid select input")
		}
		return d.chooseOption(ctx, msg.Index)

	case "nav":
		if msg.Action == "reload" || msg.Action == "back" || msg.Action == "forward" {
			d.record(recordedAction{Kind: "nav", Action: msg.Action})
		}
		switch msg.Action {
		case "reload":
			return d.callPage(ctx, "Page.reload", nil, nil)
		case "back", "forward":
			return d.goHistory(ctx, msg.Action)
		default:
			return errors.New("invalid nav input")
		}

	default:
		return fmt.Errorf("unknown input type %q", msg.T)
	}
}

const (
	pageSwapRetries    = 20
	pageSwapRetryDelay = 50 * time.Millisecond
)

// callPage sends a Page-level command, retrying while Chrome moves the page into a new renderer. During that
// swap, which follows most navigations, Chrome refuses Page commands with "Not attached to an active page",
// so a Back pressed right after a form submit would otherwise be lost.
func (d *inputDispatcher) callPage(ctx context.Context, method string, params any, result any) error {
	for attempt := 0; ; attempt++ {
		err := d.cdp.call(ctx, d.pageSession, method, params, result)
		if err == nil || attempt >= pageSwapRetries || !strings.Contains(err.Error(), "Not attached to an active page") {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pageSwapRetryDelay):
		}
	}
}

func (d *inputDispatcher) goHistory(ctx context.Context, direction string) error {
	var history struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if err := d.callPage(ctx, "Page.getNavigationHistory", nil, &history); err != nil {
		return err
	}
	index := history.CurrentIndex - 1
	if direction == "forward" {
		index = history.CurrentIndex + 1
	}
	if index < 0 || index >= len(history.Entries) {
		return nil
	}
	return d.callPage(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": history.Entries[index].ID}, nil)
}
