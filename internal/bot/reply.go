package bot

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// ReplyRelation makes a reply to cmd. Replies stay in the main timeline; only a command
// that was itself sent inside a thread is answered in that thread.
func ReplyRelation(cmd *event.Event) *event.RelatesTo {
	if msg := cmd.Content.AsMessage(); msg != nil {
		if root := msg.RelatesTo.GetThreadParent(); root != "" {
			return (&event.RelatesTo{}).SetThread(root, "").SetReplyTo(cmd.ID)
		}
	}
	return (&event.RelatesTo{}).SetReplyTo(cmd.ID)
}

// reply sends a Markdown message in reply to evt.
func (b *Bot) reply(evt *event.Event, markdown string) {
	b.send(evt, b.content(markdown))
}

// replyRich sends one or more rich (HTML table) messages in reply to evt.
func (b *Bot) replyRich(evt *event.Event, msgs ...*rich) {
	for _, m := range msgs {
		b.send(evt, m.Content())
	}
}

func (b *Bot) send(evt *event.Event, c *event.MessageEventContent) {
	c.RelatesTo = ReplyRelation(evt)
	if _, err := b.client.SendMessageEvent(b.ctx, b.roomID, event.EventMessage, c); err != nil {
		b.log.Err(err).Msg("Failed to send reply")
	}
}

// react adds an emoji reaction to evt (best effort).
func (b *Bot) react(evt *event.Event, key string) {
	_, _ = b.client.SendReaction(b.ctx, b.roomID, evt.ID, key)
}

// progress marks evt with ⏳ and returns a function that replaces it with ✅ or ❌.
func (b *Bot) progress(evt *event.Event) (done func(ok bool)) {
	resp, err := b.client.SendReaction(b.ctx, b.roomID, evt.ID, "⏳")
	return func(ok bool) {
		if err == nil {
			_, _ = b.client.RedactEvent(b.ctx, b.roomID, resp.EventID)
		}
		if ok {
			b.react(evt, "✅")
		} else {
			b.react(evt, "❌")
		}
	}
}

// sendFile uploads data (encrypted if the room is) and posts it as a file in reply to evt.
func (b *Bot) sendFile(evt *event.Event, name, mime string, data []byte) error {
	c := &event.MessageEventContent{
		MsgType:  event.MsgFile,
		Body:     name,
		FileName: name,
		Info:     &event.FileInfo{MimeType: mime, Size: len(data)},
	}
	encrypted, _ := b.client.StateStore.IsEncrypted(b.ctx, b.roomID)
	if encrypted {
		f := attachment.NewEncryptedFile()
		resp, err := b.client.UploadBytes(b.ctx, f.Encrypt(data), "application/octet-stream")
		if err != nil {
			return err
		}
		c.File = &event.EncryptedFileInfo{EncryptedFile: *f, URL: resp.ContentURI.CUString()}
	} else {
		resp, err := b.client.UploadBytes(b.ctx, data, mime)
		if err != nil {
			return err
		}
		c.URL = resp.ContentURI.CUString()
	}
	c.RelatesTo = ReplyRelation(evt)
	_, err := b.client.SendMessageEvent(b.ctx, b.roomID, event.EventMessage, c)
	return err
}

// maxImportSize caps files downloaded by the import command.
const maxImportSize = 10 << 20

// fetchFile downloads (and decrypts) the file attached to the event evtID.
func (b *Bot) fetchFile(ctx context.Context, evtID id.EventID) (name string, data []byte, err error) {
	evt, err := b.client.GetEvent(ctx, b.roomID, evtID)
	if err != nil {
		return "", nil, fmt.Errorf("fetch replied-to message: %w", err)
	}
	if err := evt.Content.ParseRaw(evt.Type); err != nil && evt.Content.Parsed == nil {
		return "", nil, fmt.Errorf("parse replied-to message: %w", err)
	}
	if evt.Type == event.EventEncrypted {
		if evt, err = b.crypto.Decrypt(ctx, evt); err != nil {
			return "", nil, fmt.Errorf("decrypt replied-to message: %w", err)
		}
	}
	msg := evt.Content.AsMessage()
	if msg == nil || (msg.MsgType != event.MsgFile && msg.MsgType != event.MsgText) || (msg.URL == "" && msg.File == nil) {
		return "", nil, fmt.Errorf("the replied-to message is not a file")
	}
	if msg.Info != nil && msg.Info.Size > maxImportSize {
		return "", nil, fmt.Errorf("file is too large (max %d MiB)", maxImportSize>>20)
	}
	uriStr := msg.URL
	if msg.File != nil {
		uriStr = msg.File.URL
	}
	uri, err := uriStr.Parse()
	if err != nil {
		return "", nil, fmt.Errorf("bad file URL: %w", err)
	}
	data, err = b.client.DownloadBytes(ctx, uri)
	if err != nil {
		return "", nil, fmt.Errorf("download file: %w", err)
	}
	if len(data) > maxImportSize {
		return "", nil, fmt.Errorf("file is too large (max %d MiB)", maxImportSize>>20)
	}
	if msg.File != nil {
		if err := msg.File.DecryptInPlace(data); err != nil {
			return "", nil, fmt.Errorf("decrypt file: %w", err)
		}
	}
	name = msg.FileName
	if name == "" {
		name = msg.Body
	}
	return name, data, nil
}
