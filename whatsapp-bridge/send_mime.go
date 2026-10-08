package main

import (
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow"
)

// Cached images and videos have category-based names, including files written
// by older bridges. Their bytes identify the MIME sent with an upload.
// Other extensions retain their existing upload category even when their bytes
// resemble an image; sniffing corrects MIME within the image/video categories.
func classifyMediaData(mediaPath string, data []byte) (whatsmeow.MediaType, string, string) {
	uploadType, mimeType, category := classifyMediaPath(mediaPath)
	if category != "image" && category != "video" {
		return uploadType, mimeType, category
	}
	sniffed := http.DetectContentType(data)
	if category == "video" && len(data) >= 12 && string(data[4:8]) == "ftyp" && string(data[8:12]) == "qt  " {
		sniffed = "video/quicktime"
	}
	if strings.HasPrefix(sniffed, category+"/") {
		mimeType = sniffed
	}
	return uploadType, mimeType, category
}
