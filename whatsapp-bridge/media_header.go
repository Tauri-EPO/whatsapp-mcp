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
func structuredHeaderMedia(msg *waE2E.Message) (string, cdnMedia) {
	template := msg.GetTemplateMessage()
	for _, header := range []mediaHeader{
		template.GetHydratedTemplate(),
		template.GetHydratedFourRowTemplate(),
		template.GetFourRowTemplate(),
		msg.GetButtonsMessage(),
		msg.GetInteractiveMessage().GetHeader(),
		template.GetInteractiveMessageTemplate().GetHeader(),
	} {
		switch {
		case header.GetDocumentMessage() != nil:
			return "document", header.GetDocumentMessage()
		case header.GetImageMessage() != nil:
			return "image", header.GetImageMessage()
		case header.GetVideoMessage() != nil:
			return "video", header.GetVideoMessage()
		}
	}
	return "", nil
}
