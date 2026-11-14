package bot

import (
	"context"
	"maps"

	"maunium.net/go/mautrix/event"
)

var (
	widgetEventType = event.Type{Type: "im.vector.modular.widgets", Class: event.StateEventType}
	layoutEventType = event.Type{Type: "io.element.widgets.layout", Class: event.StateEventType}
)

const widgetID = "archivist"

// WidgetContent is the room widget pointing at the file browser.
func WidgetContent(publicURL, name, creator string) map[string]any {
	return map[string]any{
		"id":                widgetID,
		"type":              "m.custom",
		"url":               publicURL + "/browse/",
		"name":              name,
		"creatorUserId":     creator,
		"waitForIframeLoad": true,
		"data":              map[string]any{"title": "Repository archive"},
	}
}

// syncWidget adds, updates or removes the file-browser widget according to the config.
// It only sends state events when something actually changes.
func (b *Bot) syncWidget(ctx context.Context) {
	h := b.cfg.HTTP
	want := h.Enabled && h.Widget.Enabled && h.PublicURL != ""
	if h.Enabled && h.Widget.Enabled && h.PublicURL == "" {
		b.log.Warn().Msg("http.widget is enabled but http.public_url is not set; not adding the widget")
	}

	var current map[string]any
	_ = b.client.StateEvent(ctx, b.roomID, widgetEventType, widgetID, &current)
	exists := current["url"] != nil && current["url"] != ""

	if want {
		content := WidgetContent(h.PublicURL, h.Widget.Name, b.client.UserID.String())
		if !exists || current["url"] != content["url"] || current["name"] != content["name"] {
			if _, err := b.client.SendStateEvent(ctx, b.roomID, widgetEventType, widgetID, content); err != nil {
				b.log.Warn().Err(err).Msg("Failed to add room widget (the bot needs permission to change widgets)")
				return
			}
			b.log.Info().Str("url", content["url"].(string)).Msg("Room widget added")
		}
	} else if exists {
		if _, err := b.client.SendStateEvent(ctx, b.roomID, widgetEventType, widgetID, map[string]any{}); err != nil {
			b.log.Warn().Err(err).Msg("Failed to remove room widget")
		} else {
			b.log.Info().Msg("Room widget removed")
		}
	}
	b.syncLayout(ctx, want && h.Widget.Pin)
}

// syncLayout pins the widget to the top of the room (Element's layout state), keeping the
// layout of any other widgets.
func (b *Bot) syncLayout(ctx context.Context, pin bool) {
	var layout map[string]any
	_ = b.client.StateEvent(ctx, b.roomID, layoutEventType, "", &layout)
	widgets, _ := layout["widgets"].(map[string]any)
	_, pinned := widgets[widgetID]
	if pin == pinned {
		return
	}
	newWidgets := map[string]any{}
	maps.Copy(newWidgets, widgets)
	if pin {
		newWidgets[widgetID] = map[string]any{"container": "top", "index": 0, "width": 100, "height": 40}
	} else {
		delete(newWidgets, widgetID)
	}
	newLayout := map[string]any{}
	maps.Copy(newLayout, layout)
	newLayout["widgets"] = newWidgets
	if _, err := b.client.SendStateEvent(ctx, b.roomID, layoutEventType, "", newLayout); err != nil {
		b.log.Warn().Err(err).Msg("Failed to update the room's widget layout")
	}
}
