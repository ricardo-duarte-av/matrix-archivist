package bot

import (
	"context"
	"maps"

	"maunium.net/go/mautrix/event"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
)

var (
	widgetEventType = event.Type{Type: "im.vector.modular.widgets", Class: event.StateEventType}
	layoutEventType = event.Type{Type: "io.element.widgets.layout", Class: event.StateEventType}
)

// roomWidget is one of the bot's widgets.
type roomWidget struct {
	id, path, title string
	want            bool
	cfg             config.Widget
}

const (
	widgetID         = "archivist"
	activityWidgetID = "archivist-activity"
)

// WidgetContent is the state content of a room widget pointing at publicURL+path.
func WidgetContent(id, url, name, title, creator string) map[string]any {
	return map[string]any{
		"id":                id,
		"type":              "m.custom",
		"url":               url,
		"name":              name,
		"creatorUserId":     creator,
		"waitForIframeLoad": true,
		"data":              map[string]any{"title": title},
	}
}

func (b *Bot) widgets() []roomWidget {
	h := b.cfg.HTTP
	ok := h.Enabled && h.PublicURL != ""
	return []roomWidget{
		{widgetID, "/browse/", "Repository archive", ok && h.Widget.Enabled, h.Widget},
		{activityWidgetID, "/live/", "What the archivist is doing", ok && h.Live && h.ActivityWidget.Enabled, h.ActivityWidget},
	}
}

// syncWidget adds, updates or removes the bot's widgets according to the config. It only
// sends state events when something actually changes.
func (b *Bot) syncWidget(ctx context.Context) {
	h := b.cfg.HTTP
	if h.Enabled && (h.Widget.Enabled || h.ActivityWidget.Enabled) && h.PublicURL == "" {
		b.log.Warn().Msg("http.widget / http.activity_widget are enabled but http.public_url is not set; not adding widgets")
	}
	pins := map[string]bool{}
	for _, w := range b.widgets() {
		b.syncOneWidget(ctx, w)
		pins[w.id] = w.want && w.cfg.Pin
	}
	b.syncLayout(ctx, pins)
}

func (b *Bot) syncOneWidget(ctx context.Context, w roomWidget) {
	var current map[string]any
	_ = b.client.StateEvent(ctx, b.roomID, widgetEventType, w.id, &current)
	exists := current["url"] != nil && current["url"] != ""
	if w.want {
		content := WidgetContent(w.id, b.cfg.HTTP.PublicURL+w.path, w.cfg.Name, w.title, b.client.UserID.String())
		if !exists || current["url"] != content["url"] || current["name"] != content["name"] {
			if _, err := b.client.SendStateEvent(ctx, b.roomID, widgetEventType, w.id, content); err != nil {
				b.log.Warn().Err(err).Str("widget", w.id).Msg("Failed to add room widget (the bot needs permission to change widgets)")
				return
			}
			b.log.Info().Str("widget", w.id).Str("url", content["url"].(string)).Msg("Room widget added")
		}
	} else if exists {
		if _, err := b.client.SendStateEvent(ctx, b.roomID, widgetEventType, w.id, map[string]any{}); err != nil {
			b.log.Warn().Err(err).Str("widget", w.id).Msg("Failed to remove room widget")
		} else {
			b.log.Info().Str("widget", w.id).Msg("Room widget removed")
		}
	}
}

// Layout computes Element's widget layout: the bot's pinned widgets share the top container
// side by side, and widgets that aren't the bot's keep their place.
func Layout(current map[string]any, pins map[string]bool, order []string) (map[string]any, bool) {
	widgets, _ := current["widgets"].(map[string]any)
	newWidgets := map[string]any{}
	maps.Copy(newWidgets, widgets)
	var pinned []string
	for _, id := range order {
		delete(newWidgets, id)
		if pins[id] {
			pinned = append(pinned, id)
		}
	}
	for i, id := range pinned {
		newWidgets[id] = map[string]any{"container": "top", "index": i, "width": 100 / len(pinned), "height": 40}
	}
	// Changed only if the set of pinned bot widgets differs (don't fight users resizing them).
	changed := false
	for _, id := range order {
		_, was := widgets[id]
		if was != pins[id] {
			changed = true
		}
	}
	if !changed {
		return current, false
	}
	out := map[string]any{}
	maps.Copy(out, current)
	out["widgets"] = newWidgets
	return out, true
}

func (b *Bot) syncLayout(ctx context.Context, pins map[string]bool) {
	var layout map[string]any
	_ = b.client.StateEvent(ctx, b.roomID, layoutEventType, "", &layout)
	newLayout, changed := Layout(layout, pins, []string{widgetID, activityWidgetID})
	if !changed {
		return
	}
	if _, err := b.client.SendStateEvent(ctx, b.roomID, layoutEventType, "", newLayout); err != nil {
		b.log.Warn().Err(err).Msg("Failed to update the room's widget layout")
	}
}
