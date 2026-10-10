package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

const mediaAutoTypesEnv = "WHATSAPP_MEDIA_AUTODOWNLOAD_TYPES"

func parseAutoTypes(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	set := map[string]bool{}
	for _, kind := range strings.Split(raw, ",") {
		kind = strings.TrimSpace(kind)
		if !slices.Contains(purgeMediaTypes, kind) {
			return nil, errors.New("invalid WHATSAPP_MEDIA_AUTODOWNLOAD_TYPES: expected image, video, audio, document or sticker")
		}
		set[kind] = true
	}
	return sortedNames(set), nil
}

func autoTypesDefinition() settingDefinition {
	return settingDefinition{"media.autodownload_types", mediaAutoTypesEnv, []string{"audio", "document", "image", "sticker", "video"}, func(raw json.RawMessage) (any, error) {
		var names []string
		if json.Unmarshal(raw, &names) != nil || names == nil {
			return nil, errors.New("expected type array")
		}
		for _, name := range names {
			if name == "" || strings.Contains(name, ",") {
				return nil, errors.New("invalid media type")
			}
		}
		return parseAutoTypes(strings.Join(names, ","))
	}, func(raw string) (any, error) { return parseAutoTypes(raw) }}
}

func (b *Bridge) shouldAutoCache(ctx context.Context, kind string) bool {
	if b.RuntimeDefaults == nil {
		return true
	}
	snapshot, err := b.settingsSnapshot(ctx)
	if err != nil {
		return false
	}
	setting, ok := snapshot.Settings["media.autodownload_types"]
	if !ok {
		return true
	}
	return slices.Contains(setting.Value.([]string), kind)
}
