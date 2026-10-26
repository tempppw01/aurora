package handler

import (
	"aurora/internal/chatgpt"
	"log"
	"sync"
	"time"
)

// SessionManager 按 conversationID 缓存 ChatClientState，
// 使得同一对话的多轮请求复用相同的 DeviceID / SessionID。
type SessionManager struct {
	mu               sync.RWMutex
	sessions         map[string]*sessionEntry
	responseSessions map[string]*responseSessionEntry
	ttl              time.Duration
}

type responseSessionEntry struct {
	state     chatgpt.ChatClientState
	callNames map[string]string
	lastUsed  time.Time
}

type sessionEntry struct {
	state    *chatgpt.ChatClientState
	lastUsed time.Time
}

const defaultSessionTTL = 30 * time.Minute

func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		sessions:         make(map[string]*sessionEntry),
		responseSessions: make(map[string]*responseSessionEntry),
		ttl:              defaultSessionTTL,
	}
	go sm.cleanupLoop()
	return sm
}

func (sm *SessionManager) Get(conversationID string) *chatgpt.ChatClientState {
	if conversationID == "" {
		return nil
	}
	sm.mu.RLock()
	entry, ok := sm.sessions[conversationID]
	sm.mu.RUnlock()
	if !ok {
		return nil
	}
	sm.mu.Lock()
	entry.lastUsed = time.Now()
	sm.mu.Unlock()
	return entry.state
}

func (sm *SessionManager) Register(conversationID string, state *chatgpt.ChatClientState) {
	if conversationID == "" || state == nil {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sessions[conversationID] = &sessionEntry{
		state:    state,
		lastUsed: time.Now(),
	}
}

func (sm *SessionManager) RegisterResponse(responseID string, state *chatgpt.ChatClientState, callNames map[string]string) {
	if responseID == "" || state == nil {
		return
	}
	names := make(map[string]string, len(callNames))
	for callID, name := range callNames {
		names[callID] = name
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.responseSessions[responseID] = &responseSessionEntry{
		state:     *state,
		callNames: names,
		lastUsed:  time.Now(),
	}
}

func (sm *SessionManager) GetResponse(responseID string) (*chatgpt.ChatClientState, map[string]string) {
	if responseID == "" {
		return nil, nil
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	entry, ok := sm.responseSessions[responseID]
	if !ok {
		return nil, nil
	}
	entry.lastUsed = time.Now()
	names := make(map[string]string, len(entry.callNames))
	for callID, name := range entry.callNames {
		names[callID] = name
	}
	state := entry.state
	return &state, names
}

func (sm *SessionManager) HasResponse(responseID string) bool {
	state, _ := sm.GetResponse(responseID)
	return state != nil
}

func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		sm.mu.Lock()
		now := time.Now()
		removed := 0
		for convID, entry := range sm.sessions {
			if now.Sub(entry.lastUsed) > sm.ttl {
				delete(sm.sessions, convID)
				removed++
			}
		}
		for responseID, entry := range sm.responseSessions {
			if now.Sub(entry.lastUsed) > sm.ttl {
				delete(sm.responseSessions, responseID)
				removed++
			}
		}
		if removed > 0 {
			log.Printf("[session] 清理过期 session %d 个，当前活跃 %d 个", removed, len(sm.sessions)+len(sm.responseSessions))
		}
		sm.mu.Unlock()
	}
}
