package main

import (
	"encoding/json"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// mediaPresentation retains recipient-visible metadata, without copying CDN
// credentials. NULL on old rows means it cannot be reconstructed from history.
type mediaPresentation struct {
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
	directPath   string
	presentation *mediaPresentation
}

func mediaPresentationOf(msg *waE2E.Message) *mediaPresentation {
	_, part := mediaPartOf(msg)
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
		p.Waveform = append([]byte(nil), m.Waveform...)
	case *waE2E.StickerMessage:
		p.Animated = m.IsAnimated
	}
	if p.MIME == "" && p.Name == nil && p.Title == nil && p.PTT == nil && p.Seconds == nil && len(p.Waveform) == 0 && p.Animated == nil {
		return nil
	}
	return p
}

func (p *mediaPresentation) column() any {
	if p == nil {
		return nil
	}
	// This concrete type contains only JSON-supported scalar fields and bytes.
	data, _ := json.Marshal(p)
	return string(data)
}
