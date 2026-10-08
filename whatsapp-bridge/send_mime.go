package main

import (
	"context"
	"go.mau.fi/whatsmeow"
)

// Only the forward handler knows that its path has a category cache name.
type cachedForwardMIMEKey struct{}

func classifySendMedia(ctx context.Context, mediaPath string, data []byte) (whatsmeow.MediaType, string, string) {
	if cached, _ := ctx.Value(cachedForwardMIMEKey{}).(bool); cached {
		return classifyMediaData(mediaPath, data)
	}
	return classifyMediaPath(mediaPath)
}

// Cached images/videos have category names (.jpg/.mp4), regardless of bytes.
// Share the webhook's byte detector, but restrict forwarding to existing wire
// types within the same category and add the leading QuickTime ftyp brand.
// Webhooks identify a data URL's broader formats and need no such restriction.
func classifyMediaData(mediaPath string, data []byte) (whatsmeow.MediaType, string, string) {
	uploadType, mimeType, category := classifyMediaPath(mediaPath)
	sniffed := sniffMIME(data)
	if category == "video" && len(data) >= 12 && string(data[4:8]) == "ftyp" && string(data[8:12]) == "qt  " {
		sniffed = "video/quicktime"
	}
	switch category {
	case "image":
		switch sniffed {
		case "image/jpeg", "image/png", "image/gif", "image/webp":
			mimeType = sniffed
		}
	case "video":
		switch sniffed {
		case "video/mp4", "video/quicktime", "video/avi":
			mimeType = sniffed
		}
	}
	return uploadType, mimeType, category
}
