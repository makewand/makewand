package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/makewand/makewand/internal/i18n"
)

type chatActivityPhase string

const (
	chatActivityPreparing chatActivityPhase = "preparing"
	chatActivityContext   chatActivityPhase = "context"
	chatActivityRouting   chatActivityPhase = "routing"
	chatActivityWaiting   chatActivityPhase = "waiting"
	chatActivityStreaming chatActivityPhase = "streaming"
)

type chatActivityState struct {
	mu sync.RWMutex

	active     bool
	phase      chatActivityPhase
	provider   string
	requested  string
	isFallback bool
	detail     string
	startedAt  time.Time
	chunkCount int
	charCount  int
}

type chatActivitySnapshot struct {
	Active     bool
	Phase      chatActivityPhase
	Provider   string
	Requested  string
	IsFallback bool
	Detail     string
	StartedAt  time.Time
	ChunkCount int
	CharCount  int
}

func newChatActivityState() *chatActivityState {
	return &chatActivityState{}
}

func (s *chatActivityState) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.active = true
	s.phase = chatActivityPreparing
	s.provider = ""
	s.requested = ""
	s.isFallback = false
	s.detail = ""
	s.startedAt = time.Now()
	s.chunkCount = 0
	s.charCount = 0
}

func (s *chatActivityState) SetPhase(phase chatActivityPhase, provider, requested string, isFallback bool, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.active {
		s.active = true
		s.startedAt = time.Now()
	}
	s.phase = phase
	if provider != "" {
		s.provider = provider
	}
	if requested != "" {
		s.requested = requested
	}
	s.isFallback = isFallback
	s.detail = strings.TrimSpace(detail)
}

func (s *chatActivityState) MarkChunk(provider string, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.active {
		s.active = true
		s.startedAt = time.Now()
	}
	s.phase = chatActivityStreaming
	if provider != "" {
		s.provider = provider
	}
	s.detail = ""
	s.chunkCount++
	s.charCount += utf8.RuneCountInString(content)
}

func (s *chatActivityState) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.active = false
	s.phase = ""
	s.provider = ""
	s.requested = ""
	s.isFallback = false
	s.detail = ""
	s.startedAt = time.Time{}
	s.chunkCount = 0
	s.charCount = 0
}

func (s *chatActivityState) Snapshot() chatActivitySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return chatActivitySnapshot{
		Active:     s.active,
		Phase:      s.phase,
		Provider:   s.provider,
		Requested:  s.requested,
		IsFallback: s.isFallback,
		Detail:     s.detail,
		StartedAt:  s.startedAt,
		ChunkCount: s.chunkCount,
		CharCount:  s.charCount,
	}
}

func formatChatActivityHeadline(s chatActivitySnapshot) string {
	msg := i18n.Msg()
	if detail := strings.TrimSpace(s.Detail); detail != "" {
		return detail
	}

	switch s.Phase {
	case chatActivityPreparing:
		return msg.ChatActivityPreparing
	case chatActivityContext:
		return msg.ChatActivityContext
	case chatActivityRouting:
		if s.Provider != "" {
			return fmt.Sprintf(msg.ChatActivitySelectingProvider, s.Provider)
		}
		return msg.ChatActivitySelectingGeneric
	case chatActivityWaiting:
		if s.Provider != "" {
			return fmt.Sprintf(msg.ChatActivityWaitingProvider, s.Provider)
		}
		return msg.ChatActivityWaitingGeneric
	case chatActivityStreaming:
		if s.Provider != "" {
			return fmt.Sprintf(msg.ChatActivityStreamingProvider, s.Provider)
		}
		return msg.ChatActivityStreamingGeneric
	default:
		return msg.ChatActivityWorking
	}
}

func formatChatActivityMeta(s chatActivitySnapshot) string {
	msg := i18n.Msg()
	var parts []string
	if !s.StartedAt.IsZero() {
		parts = append(parts, fmt.Sprintf(msg.ChatActivityElapsed, formatActivityDuration(time.Since(s.StartedAt))))
	}
	if s.IsFallback && s.Requested != "" && s.Provider != "" && s.Requested != s.Provider {
		parts = append(parts, fmt.Sprintf(msg.ChatActivityFallback, s.Requested))
	}
	if s.ChunkCount > 0 {
		if s.ChunkCount == 1 {
			parts = append(parts, fmt.Sprintf(msg.ChatActivityChunkOne, s.ChunkCount))
		} else {
			parts = append(parts, fmt.Sprintf(msg.ChatActivityChunkMany, s.ChunkCount))
		}
	}
	if s.CharCount > 0 {
		parts = append(parts, fmt.Sprintf(msg.ChatActivityChars, s.CharCount))
	}
	return strings.Join(parts, " • ")
}

func formatChatActivityStatus(s chatActivitySnapshot) string {
	headline := formatChatActivityHeadline(s)
	meta := formatChatActivityMeta(s)
	if meta == "" {
		return headline
	}
	return headline + " • " + meta
}

func formatActivityDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	}
	minutes := int(d / time.Minute)
	seconds := int((d % time.Minute) / time.Second)
	if seconds == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}
