// Package session manages per-session conversation history with hybrid compaction.
package session

import (
	"fmt"
	"sync"
	"time"

	"github.com/Whitfrost21/FlareSpark/internal/groq"
)

const (
	compactAfterTurns = 10
	keepRecentTurns   = 4
	CompactionModel   = "llama-3.1-8b-instant"
)

const systemPrompt = "You are Flarespark, a coding assistant in Zed. Be concise. Prefer code over explanation."

const CompactionPrompt = `Summarize the conversation above into a concise memory block for a coding assistant.
Focus on: decisions made, files discussed, code written, problems solved, current task context.
Be terse. Plain prose. Max 120 words.`

// Session holds conversation history and config for one ACP session.
type Session struct {
	History       []groq.Message
	Model         string
	Turns         int
	lastCompacted int // Turns value at the last compaction — prevents re-firing
	SeenURIs      []string
}

// Store is a thread-safe map of sessions.
type Store struct {
	mu   sync.Mutex
	data map[string]*Session
}

func NewStore() *Store {
	return &Store{data: make(map[string]*Session)}
}

func newSession() *Session {
	return &Session{
		History: []groq.Message{{Role: "system", Content: systemPrompt}},
		Model:   groq.DefaultModel,
	}
}

func (s *Store) New() string {
	id := fmt.Sprintf("sess-%d", time.Now().UnixNano())
	s.mu.Lock()
	s.data[id] = newSession()
	s.mu.Unlock()
	return id
}

func (s *Store) GetOrCreate(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.data[id]; ok {
		return sess
	}
	sess := newSession()
	s.data[id] = sess
	return sess
}

// AddURI records a file URI seen in this session (deduped).
func (s *Store) AddURI(id, uri string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.data[id]
	if !ok {
		return
	}
	for _, u := range sess.SeenURIs {
		if u == uri {
			return
		}
	}
	sess.SeenURIs = append(sess.SeenURIs, uri)
}

// SeenURIs returns a snapshot of all URIs recorded for this session.
func (s *Store) SeenURIs(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.data[id]
	if !ok {
		return nil
	}
	out := make([]string, len(sess.SeenURIs))
	copy(out, sess.SeenURIs)
	return out
}

// NeedsCompaction returns true once per compaction threshold crossing.
//
// BUG FIX: the old code used Turns%compactAfterTurns==0 which re-fired on
// every multiple of 10 forever (10, 20, 30...). Now we track lastCompacted
// and only fire when Turns has advanced a full compactAfterTurns since the
// last time we actually ran a compaction.
func (s *Store) NeedsCompaction(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.data[id]
	if !ok {
		return false
	}
	return sess.Turns >= sess.lastCompacted+compactAfterTurns
}

// HistoryForCompaction returns a COPY of the turns to summarise — everything
// except the system message, any existing memory block, and the most recent
// keepRecentTurns pairs.
//
// BUG FIX: the old code returned a live slice into sess.History. If the
// compaction goroutine held that slice while AppendUser or ApplyCompaction
// ran concurrently, the underlying array was modified mid-read — a data race
// that silently corrupted history.
func (s *Store) HistoryForCompaction(id string) []groq.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.data[id]
	if !ok {
		return nil
	}
	// History layout: [system] [memory?] [turns...]
	start := 1
	if len(sess.History) > 1 && sess.History[1].Role == "system" {
		start = 2
	}
	recentStart := len(sess.History) - keepRecentTurns*2
	if recentStart <= start {
		return nil
	}
	// Return a copy — caller must not hold the lock.
	src := sess.History[start:recentStart]
	out := make([]groq.Message, len(src))
	copy(out, src)
	return out
}

// MarkCompacted records that compaction ran at the current Turns value.
// Must be called by the handler after ApplyCompaction succeeds.
func (s *Store) MarkCompacted(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.data[id]; ok {
		sess.lastCompacted = sess.Turns
	}
}

// ApplyCompaction replaces the old turns with a single memory block,
// keeping the most recent keepRecentTurns pairs verbatim.
func (s *Store) ApplyCompaction(id, summary string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.data[id]
	if !ok {
		return
	}
	start := 1
	if len(sess.History) > 1 && sess.History[1].Role == "system" {
		start = 2
	}
	recentStart := len(sess.History) - keepRecentTurns*2
	if recentStart <= start {
		return
	}
	recent := sess.History[recentStart:]
	memBlock := groq.Message{
		Role:    "system",
		Content: "[Conversation memory]\n" + summary,
	}
	newHistory := make([]groq.Message, 0, 1+1+len(recent))
	newHistory = append(newHistory, sess.History[0]) // system prompt
	newHistory = append(newHistory, memBlock)
	newHistory = append(newHistory, recent...)
	sess.History = newHistory
}

// AppendUser adds a user turn and returns a history snapshot + active model.
//
// BUG FIX: the old code did sess := s.data[id] with no nil check — a panic
// if the session was missing (e.g. after an agent restart). Now we
// GetOrCreate so the session is always valid.
func (s *Store) AppendUser(id, text string) ([]groq.Message, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.data[id]
	if !ok {
		// Session missing — create a fresh one rather than panicking.
		sess = newSession()
		s.data[id] = sess
	}
	sess.History = append(sess.History, groq.Message{Role: "user", Content: text})
	snapshot := make([]groq.Message, len(sess.History))
	copy(snapshot, sess.History)
	return snapshot, sess.Model
}

// AppendAssistant persists the assistant reply and increments the turn counter.
func (s *Store) AppendAssistant(id, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.data[id]; ok {
		sess.History = append(sess.History, groq.Message{Role: "assistant", Content: text})
		sess.Turns++
	}
}

func (s *Store) SetModel(id, modelID string) error {
	if err := groq.Validate(modelID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.data[id]; ok {
		sess.Model = modelID
	}
	return nil
}
