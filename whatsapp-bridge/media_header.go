package main

import "go.mau.fi/whatsmeow/proto/waE2E"

type mediaHeader interface {
	GetDocumentMessage() *waE2E.DocumentMessage
	GetImageMessage() *waE2E.ImageMessage
	GetVideoMessage() *waE2E.VideoMessage
}

// Protobuf getters are nil-safe, including typed nil headers. Top-level media
// has already won in mediaPartOf; each structured header then supplies the same
// CDN fields and filename through the canonical extraction path.
func messageMediaHeaders(msg *waE2E.Message) []mediaHeader {
	template := msg.GetTemplateMessage()
	return []mediaHeader{
		template.GetHydratedTemplate(),
		template.GetHydratedFourRowTemplate(),
		template.GetFourRowTemplate(),
		msg.GetButtonsMessage(),
		msg.GetInteractiveMessage().GetHeader(),
		template.GetInteractiveMessageTemplate().GetHeader(),
	}
}

func structuredHeaderMedia(msg *waE2E.Message) (string, cdnMedia) {
	for _, header := range messageMediaHeaders(msg) {
		var kind string
		var part cdnMedia
		switch {
		case header.GetDocumentMessage() != nil:
			kind, part = "document", header.GetDocumentMessage()
		case header.GetImageMessage() != nil:
			kind, part = "image", header.GetImageMessage()
		case header.GetVideoMessage() != nil:
			kind, part = "video", header.GetVideoMessage()
		}
		if part != nil && (part.GetURL() != "" || part.GetDirectPath() != "") && len(part.GetMediaKey()) != 0 {
			return kind, part
		}
	}
	return "", nil
}
