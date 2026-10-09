package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"strings"
	"unicode"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// mediaPresentation retains recipient-visible metadata, without copying CDN
// credentials. NULL on old rows means it cannot be reconstructed from history.
type mediaPresentation struct {
	Hash     string  `json:"sha256,omitempty"`
	MIME     string  `json:"mime,omitempty"`
	Name     *string `json:"name,omitempty"`
	Title    *string `json:"title,omitempty"`
	PTT      *bool   `json:"ptt,omitempty"`
	Seconds  *uint32 `json:"seconds,omitempty"`
	Waveform []byte  `json:"waveform,omitempty"`
	Animated *bool   `json:"animated,omitempty"`
}

// messageMediaOptions keeps presentation and direct path in the same upsert.
type messageMediaOptions struct {
	directPath             string
	presentation           *mediaPresentation
	location               *messageLocation
	retryChat, retrySender string
}

func mediaPresentationOf(msg *waE2E.Message) *mediaPresentation {
	kind, part := mediaPartOf(msg)
	if part == nil {
		return nil
	}
	p := &mediaPresentation{}
	if m, ok := part.(interface{ GetMimetype() string }); ok {
		p.MIME = m.GetMimetype()
	}
	switch m := part.(type) {
	case *waE2E.DocumentMessage:
		p.Name, p.Title = m.FileName, m.Title
	case *waE2E.AudioMessage:
		p.PTT, p.Seconds = m.PTT, m.Seconds
		p.Waveform = m.Waveform // validated clones only an exact 64-byte waveform
	case *waE2E.StickerMessage:
		p.Animated = m.IsAnimated
	}
	if p.MIME == "" && p.Name == nil && p.Title == nil && p.PTT == nil && p.Seconds == nil && len(p.Waveform) == 0 && p.Animated == nil {
		return nil
	}
	return p.forFile(kind, part.GetFileSHA256())
}

func (p *mediaPresentation) forFile(kind string, sha []byte) *mediaPresentation {
	if p == nil || len(sha) == 0 || len(sha) > 64 {
		return nil
	}
	hash := hex.EncodeToString(sha)
	if p.Hash != "" && p.Hash != hash {
		return nil
	}
	q := p.validated(kind)
	q.Hash = hash
	return q
}

func (p *mediaPresentation) forBytes(kind string, data []byte) (*mediaPresentation, error) {
	if p != nil && p.Hash != "" {
		hash := sha256.Sum256(data)
		if p.Hash != hex.EncodeToString(hash[:]) {
			return nil, fmt.Errorf("cached media does not match its stored presentation hash")
		}
	}
	return p.validated(kind), nil
}

// Validate at ingress and again at the wire sink, including old database rows.
func (p *mediaPresentation) validated(kind string) *mediaPresentation {
	if p == nil {
		return nil
	}
	q := *p
	q.MIME = presentationMIME(kind, p.MIME)
	if p.Name != nil {
		name := cleanDisplayName(*p.Name)
		q.Name = &name
	}
	if p.Title != nil {
		title := cleanDisplayName(*p.Title)
		q.Title = &title
	}
	if p.Seconds != nil && *p.Seconds > 86400 {
		q.Seconds = nil
	}
	q.Waveform = nil
	if len(p.Waveform) == 64 {
		q.Waveform = append([]byte(nil), p.Waveform...)
	}
	return &q
}

func presentationMIME(kind, raw string) string {
	if kind == "audio" && (strings.EqualFold(raw, "audio/ogg; codecs=opus") || strings.EqualFold(raw, "audio/ogg;codecs=opus")) {
		return "audio/ogg; codecs=opus"
	}
	if len(raw) > 255 || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return ""
	}
	parsed, params, err := mime.ParseMediaType(raw)
	if err != nil || len(params) != 0 || parsed != strings.ToLower(raw) || strings.Count(parsed, "/") != 1 || strings.Contains(parsed, "*") {
		return ""
	}
	allowed := kind == "document" || (kind == "audio" && strings.HasPrefix(parsed, "audio/")) ||
		(kind == "sticker" && parsed == "image/webp") ||
		(kind == "image" && (parsed == "image/jpeg" || parsed == "image/png" || parsed == "image/gif" || parsed == "image/webp")) ||
		(kind == "video" && (parsed == "video/mp4" || parsed == "video/quicktime" || parsed == "video/avi"))
	if !allowed {
		return ""
	}
	return parsed
}

func readMediaPresentation(raw, kind string, sha []byte) *mediaPresentation {
	var p *mediaPresentation
	if len(raw) > 4096 || json.Unmarshal([]byte(raw), &p) != nil || p == nil {
		bridgeLog.Debugf("Ignoring invalid stored media presentation")
		return nil
	}
	if p.Hash == "" || p.Hash != hex.EncodeToString(sha) {
		bridgeLog.Debugf("Ignoring stored media presentation without a matching file hash")
		return nil
	}
	return p.validated(kind)
}

func (p *mediaPresentation) column() any {
	if p == nil {
		return nil
	}
	// This concrete type contains only JSON-supported scalar fields and bytes.
	data, _ := json.Marshal(p)
	return string(data)
}
