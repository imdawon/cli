package webapp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Headless Chrome draws a <select>'s open menu in a separate popup widget that the screencast never
// captures, so a click on one would open a menu nobody can see. The gateway instead sends the viewer the
// options to draw itself, and applies the choice to the real element.

const maxSelectOptions = 500

type SelectOption struct {
	Label    string `json:"label"`
	Disabled bool   `json:"disabled,omitempty"`
}

type SelectRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Returns the single-choice, enabled <select> under a point, or nothing.
const selectAtPointScript = `(() => {
  const at = document.elementFromPoint(%f, %f);
  const el = at && at.closest("select");
  return el && !el.multiple && !el.disabled ? el : null;
})()`

const describeSelectFunction = `function (max) {
  this.focus();
  const r = this.getBoundingClientRect();
  return {
    options: Array.from(this.options).slice(0, max).map((o) => ({ label: o.label, disabled: o.disabled })),
    selected: this.selectedIndex,
    rect: { x: r.left, y: r.top, w: r.width, h: r.height },
  };
}`

const chooseOptionFunction = `function (index) {
  if (index < 0 || index >= this.options.length || this.options[index].disabled) return "";
  this.selectedIndex = index;
  this.dispatchEvent(new Event("input", { bubbles: true }));
  this.dispatchEvent(new Event("change", { bubbles: true }));
  return this.options[index].label;
}`

// openSelect opens the viewer's menu for a <select> under the point, reporting whether it found one.
func (d *inputDispatcher) openSelect(ctx context.Context, x, y float64) (bool, error) {
	var found struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
	}
	if err := d.cdp.call(ctx, d.pageSession, "Runtime.evaluate", map[string]any{
		"expression": fmt.Sprintf(selectAtPointScript, x, y),
	}, &found); err != nil || found.Result.ObjectID == "" {
		return false, err
	}

	var described struct {
		Result struct {
			Value struct {
				Options  []SelectOption `json:"options"`
				Selected int            `json:"selected"`
				Rect     SelectRect     `json:"rect"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := d.cdp.call(ctx, d.pageSession, "Runtime.callFunctionOn", map[string]any{
		"objectId":            found.Result.ObjectID,
		"functionDeclaration": describeSelectFunction,
		"arguments":           []map[string]any{{"value": maxSelectOptions}},
		"returnByValue":       true,
	}, &described); err != nil {
		return false, err
	}

	d.releaseSelect(ctx)
	d.selectObjectID = found.Result.ObjectID
	value := described.Result.Value
	selected := value.Selected
	rect := value.Rect
	return true, d.notify(Event{Kind: EventKindSelect, Options: value.Options, Selected: &selected, Rect: &rect})
}

// chooseOption applies the viewer's choice (-1 dismisses the menu) to the select it opened.
func (d *inputDispatcher) chooseOption(ctx context.Context, index int) error {
	if d.selectObjectID == "" {
		return nil
	}
	defer d.releaseSelect(ctx)
	if index < 0 || index >= maxSelectOptions {
		return nil
	}

	var chosen struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if err := d.cdp.call(ctx, d.pageSession, "Runtime.callFunctionOn", map[string]any{
		"objectId":            d.selectObjectID,
		"functionDeclaration": chooseOptionFunction,
		"arguments":           []map[string]any{{"value": index}},
		"returnByValue":       true,
	}, &chosen); err != nil {
		return err
	}
	var label string
	if json.Unmarshal(chosen.Result.Value, &label) == nil && label != "" {
		d.record(recordedAction{Kind: "select", Value: label})
	}
	return nil
}

func (d *inputDispatcher) releaseSelect(ctx context.Context) {
	if d.selectObjectID == "" {
		return
	}
	_ = d.cdp.call(ctx, d.pageSession, "Runtime.releaseObject", map[string]any{"objectId": d.selectObjectID}, nil)
	d.selectObjectID = ""
}
