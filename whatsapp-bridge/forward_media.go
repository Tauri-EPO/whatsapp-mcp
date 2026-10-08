package main

import (
	"context"
	"fmt"
	"mime"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

type forwardSource struct {
	content, mediaType, filename, id string
	presentation                     *mediaPresentation
}

type forwardSourceKey struct{}

func forwardMediaType(ctx context.Context, path string, data []byte) (whatsmeow.MediaType, string, error) {
	source, ok := ctx.Value(forwardSourceKey{}).(*forwardSource)
	if !ok {
		kind, contentType, _ := classifySendMedia(ctx, path, data)
		return kind, contentType, nil
	}
	var upload whatsmeow.MediaType
	var contentType string
	switch source.mediaType {
	case "document":
		upload = whatsmeow.MediaDocument
		_, contentType, _ = classifyMediaPath(source.filename)
	case "sticker":
		upload, contentType = whatsmeow.MediaImage, sniffMIME(data)
		if isWebP(data) {
			contentType = "image/webp"
		}
	case "audio":
		upload, contentType = whatsmeow.MediaAudio, sniffMIME(data)
		switch contentType {
		case "video/mp4":
			contentType = "audio/mp4"
		case "audio/wave":
			contentType = "audio/wav"
		case "application/ogg":
			contentType = "audio/ogg"
		}
		if len(data) >= 7 && data[0] == 0xff && data[1]&0xf6 == 0xf0 {
			contentType = "audio/aac"
		}
		// MPEG audio frames without an ID3 tag: sync/version/layer, a real
		// bitrate index and sample rate (ADTS has no MPEG audio layer).
		if len(data) >= 4 && data[0] == 0xff && data[1]&0xe0 == 0xe0 && data[1]&0x18 != 0x08 && data[1]&0x06 != 0 && data[2]&0xf0 != 0 && data[2]&0xf0 != 0xf0 && data[2]&0x0c != 0x0c {
			contentType = "audio/mpeg"
		}
	case "image", "video":
		upload, contentType, _ = classifyMediaData(path, data)
	default:
		return "", "", fmt.Errorf("unsupported forwarded media kind: %s", source.mediaType)
	}
	if p := source.presentation.validated(source.mediaType); p != nil && p.MIME != "" && source.mediaType != "image" && source.mediaType != "video" && (source.mediaType != "sticker" || isWebP(data)) {
		contentType = p.MIME
	}
	if source.mediaType == "audio" {
		parsed, _, err := mime.ParseMediaType(contentType)
		if err != nil || !strings.HasPrefix(parsed, "audio/") {
			return "", "", fmt.Errorf("cannot determine stored audio codec")
		}
	}
	return upload, contentType, nil
}

func buildForwardMedia(ctx context.Context, kind whatsmeow.MediaType, contentType, path string, data []byte, upload whatsmeow.UploadResponse, caption string, quote outboundQuote, mentions []string) (*waE2E.Message, string, error) {
	source, ok := ctx.Value(forwardSourceKey{}).(*forwardSource)
	if !ok {
		return buildOutboundMedia(kind, contentType, path, data, upload, caption, quote, mentions)
	}
	p := source.presentation.validated(source.mediaType)
	switch source.mediaType {
	case "audio":
		audio := &waE2E.AudioMessage{Mimetype: proto.String(contentType), URL: &upload.URL, DirectPath: &upload.DirectPath, MediaKey: upload.MediaKey, FileSHA256: upload.FileSHA256, FileEncSHA256: upload.FileEncSHA256, FileLength: &upload.FileLength, PTT: proto.Bool(false)}
		if p != nil {
			audio.PTT, audio.Seconds, audio.Waveform = p.PTT, p.Seconds, p.Waveform
		}
		if isOggOpus(data) {
			if p == nil {
				audio.PTT = proto.Bool(true)
			}
			if audio.Seconds == nil || len(audio.Waveform) != 64 {
				seconds, waveform, err := analyzeOggOpus(data)
				if err != nil {
					return nil, "", err
				}
				if audio.Seconds == nil && seconds <= 86400 {
					audio.Seconds = proto.Uint32(seconds)
				}
				if len(audio.Waveform) != 64 {
					audio.Waveform = waveform
				}
			}
		}
		return &waE2E.Message{AudioMessage: audio}, "", nil
	case "sticker":
		if !isWebP(data) {
			return buildOutboundMedia(whatsmeow.MediaImage, contentType, path, data, upload, caption, quote, mentions)
		}
		sticker := &waE2E.StickerMessage{Mimetype: proto.String(contentType), URL: &upload.URL, DirectPath: &upload.DirectPath, MediaKey: upload.MediaKey, FileSHA256: upload.FileSHA256, FileEncSHA256: upload.FileEncSHA256, FileLength: &upload.FileLength}
		if p != nil {
			sticker.IsAnimated = p.Animated
		}
		return &waE2E.Message{StickerMessage: sticker}, "", nil
	case "document":
		name, title := source.filename, source.filename
		if p != nil {
			title = ""
			if p.Name != nil {
				name = *p.Name
			}
			if p.Title != nil {
				title = *p.Title
			}
		}
		// Legacy unnamed documents used a generated name in filename. Never
		// expose that source ID/timestamp or the on-disk cache basename.
		if (p == nil || p.Name == nil) && generatedMediaName(name) {
			name, title = "", ""
		}
		msg, text, err := buildOutboundMedia(kind, contentType, name, data, upload, caption, quote, mentions)
		if err == nil {
			msg.DocumentMessage.Title = proto.String(cleanOutboundName(title))
		}
		return msg, text, err
	default:
		return buildOutboundMedia(kind, contentType, path, data, upload, caption, quote, mentions)
	}
}

func isWebP(data []byte) bool {
	return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

func isOggOpus(data []byte) bool {
	if len(data) < 27 || string(data[:4]) != "OggS" || data[26] == 0 || data[5]&1 != 0 {
		return false
	}
	start := 27 + int(data[26])
	return start+19 <= len(data) && data[27] >= 19 && string(data[start:start+8]) == "OpusHead"
}

func generatedMediaName(name string) bool {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	for _, kind := range []string{"document", "image", "video", "audio", "sticker"} {
		if !strings.HasPrefix(name, kind+"_") {
			continue
		}
		rest := strings.TrimPrefix(name, kind+"_")
		if len(rest) < 15 {
			return false
		}
		if _, err := time.Parse("20060102_150405", rest[:15]); err != nil {
			return false
		}
		suffix := rest[15:]
		return suffix == "" || ((suffix[0] == '_' || suffix[0] == '.') && len(suffix) > 1)
	}
	return false
}
