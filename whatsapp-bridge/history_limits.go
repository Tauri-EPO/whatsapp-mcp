package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"google.golang.org/protobuf/proto"
)

const (
	historySyncDaysEnv  = "WHATSAPP_HISTORY_SYNC_DAYS"
	historySyncSizeEnv  = "WHATSAPP_HISTORY_SYNC_SIZE_MB"
	historySyncQuotaEnv = "WHATSAPP_HISTORY_SYNC_STORAGE_QUOTA_MB"
)

type historyLimits struct {
	Days, SizeMB, QuotaMB uint32
	MaxAge                time.Duration
	WarnBytes             int64
}

func parseHistoryLimits(getenv func(string) string) (historyLimits, error) {
	var cfg historyLimits
	var problems []string
	for _, field := range []struct {
		name   string
		target *uint32
	}{
		{historySyncDaysEnv, &cfg.Days},
		{historySyncSizeEnv, &cfg.SizeMB},
		{historySyncQuotaEnv, &cfg.QuotaMB},
	} {
		if value := getenv(field.name); value != "" {
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil || n == 0 {
				problems = append(problems, fmt.Sprintf("%s must be a positive 32-bit integer", field.name))
				continue
			}
			*field.target = uint32(n)
		}
	}
	if value := getenv("WHATSAPP_HISTORY_MAX_AGE_DAYS"); value != "" {
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil || n == 0 || n > uint64((1<<63-1)/(24*time.Hour)) {
			problems = append(problems, "WHATSAPP_HISTORY_MAX_AGE_DAYS must be positive and fit a duration")
		} else {
			cfg.MaxAge = time.Duration(n) * 24 * time.Hour
		}
	}
	if value := getenv("WHATSAPP_STORE_WARN_BYTES"); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n <= 0 {
			problems = append(problems, "WHATSAPP_STORE_WARN_BYTES must be a positive integer")
		} else {
			cfg.WarnBytes = n
		}
	}
	if len(problems) > 0 {
		return cfg, errors.New(strings.Join(problems, "; "))
	}
	return cfg, nil
}

func (cfg historyLimits) pairingConfigured() bool {
	return cfg.Days != 0 || cfg.SizeMB != 0 || cfg.QuotaMB != 0
}

func (cfg historyLimits) validateFlag(full bool) error {
	if full && cfg.pairingConfigured() {
		return errors.New("--full-history-pair cannot be combined with WHATSAPP_HISTORY_SYNC_DAYS, WHATSAPP_HISTORY_SYNC_SIZE_MB or WHATSAPP_HISTORY_SYNC_STORAGE_QUOTA_MB")
	}
	return nil
}

// Called before any pairing handshake; retain the SDK defaults for omitted fields.
func (cfg historyLimits) apply(props *waCompanionReg.DeviceProps, paired, full bool) {
	if paired || (!full && !cfg.pairingConfigured()) {
		return
	}
	if full {
		cfg.Days, cfg.SizeMB, cfg.QuotaMB = 3650, 102400, 102400
	}
	props.RequireFullSync = proto.Bool(true)
	if props.HistorySyncConfig == nil {
		props.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{}
	}
	if cfg.Days != 0 {
		props.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(cfg.Days)
	}
	if cfg.SizeMB != 0 {
		props.HistorySyncConfig.FullSyncSizeMbLimit = proto.Uint32(cfg.SizeMB)
	}
	if cfg.QuotaMB != 0 {
		props.HistorySyncConfig.StorageQuotaMb = proto.Uint32(cfg.QuotaMB)
	}
}

type historyStatus struct {
	State         string `json:"state"`
	Progress      uint32 `json:"progress"`
	Conversations int64  `json:"conversations"`
	Messages      int64  `json:"messages"`
}
type historyProgress struct {
	mu     sync.Mutex
	status historyStatus
}

func (p *historyProgress) snapshot() historyStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.status
	if s.State == "" {
		s.State = "idle"
	}
	return s
}
func (p *historyProgress) update(kind waHistorySync.HistorySync_HistorySyncType, progress uint32, conversations int, messages int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch kind {
	case waHistorySync.HistorySync_INITIAL_BOOTSTRAP, waHistorySync.HistorySync_RECENT, waHistorySync.HistorySync_FULL:
		if p.status.State != "complete" {
			p.status.State, p.status.Progress = "syncing", max(p.status.Progress, min(progress, 100))
			if progress >= 100 {
				p.status.State = "complete"
			}
		}
	}
	p.status.Conversations += int64(conversations)
	p.status.Messages += messages
}

// Filter a local slice, never mutate the SDK's payload. Do this before side
// effects, including edit/vote processing and nested history-share downloads.
func (b *Bridge) boundedHistory(data *waHistorySync.HistorySync, now time.Time) *waHistorySync.HistorySync {
	if data == nil || b.HistoryLimits.MaxAge <= 0 {
		return data
	}
	cutoff := now.Add(-b.HistoryLimits.MaxAge).Unix()
	copyData := &waHistorySync.HistorySync{SyncType: data.SyncType, ChunkOrder: data.ChunkOrder, Progress: data.Progress}
	for _, conversation := range data.Conversations {
		if conversation == nil {
			continue
		}
		var rows []*waHistorySync.HistorySyncMsg
		for start := 0; start < len(conversation.Messages); start += historyBatchMessages {
			dropped := 0
			for _, row := range conversation.Messages[start:min(start+historyBatchMessages, len(conversation.Messages))] {
				if row != nil && row.Message != nil && row.Message.GetMessageTimestamp() > 0 && row.Message.GetMessageTimestamp() < uint64(max(cutoff, 0)) {
					dropped++
					continue
				}
				rows = append(rows, row)
			}
			if dropped > 0 {
				b.metrics.historyDropped.Add(int64(dropped))
				b.Log.Infof("History age guard: dropped %d messages in chunk", dropped)
			}
		}
		if len(rows) > 0 {
			copyConversation := &waHistorySync.Conversation{ID: conversation.ID, Name: conversation.Name, DisplayName: conversation.DisplayName, UnreadCount: conversation.UnreadCount, MarkedAsUnread: conversation.MarkedAsUnread, EphemeralExpiration: conversation.EphemeralExpiration, EphemeralSettingTimestamp: conversation.EphemeralSettingTimestamp, Messages: rows}
			copyData.Conversations = append(copyData.Conversations, copyConversation)
		}
	}
	return copyData
}
