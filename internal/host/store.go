package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// store keeps conversations on disk: index.json lists their metadata and
// conversations/<id>.json holds each transcript. Writes are atomic
// (temp file + rename) and transcripts are written at most every 400ms
// while an agent streams.
type store struct {
	dir string

	flushMu sync.Mutex
	mu      sync.Mutex
	metas   map[string]*Meta
	events  map[string][]Event // loaded transcripts
	dirty   map[string]bool
	/** Conversations deleted here, so a merge with the file doesn't bring them back. */
	gone            map[string]bool
	timer           *time.Timer
	timerGeneration uint64
}

const (
	indexKey        = "\x00index"
	flushDelay      = 400 * time.Millisecond
	flushRetryDelay = 2 * time.Second
)

func openStore(dir string) (*store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "conversations"), 0o755); err != nil {
		return nil, err
	}
	s := &store{dir: dir, metas: map[string]*Meta{}, events: map[string][]Event{}, dirty: map[string]bool{}, gone: map[string]bool{}}
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err == nil {
		var list []*Meta
		if json.Unmarshal(b, &list) == nil {
			for _, m := range list {
				if m != nil && m.ID != "" {
					m.Status = "idle"
					m.Pending = 0
					s.metas[m.ID] = m
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

func (s *store) list() []Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Meta, 0, len(s.metas))
	for _, m := range s.metas {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

func (s *store) meta(id string) (Meta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metas[id]
	if !ok {
		return Meta{}, false
	}
	return *m, true
}

// update changes a conversation's metadata under the lock and returns the result.
func (s *store) update(id string, fn func(m *Meta)) (Meta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metas[id]
	if !ok {
		return Meta{}, false
	}
	fn(m)
	s.dirty[indexKey] = true
	s.schedule(flushDelay)
	return *m, true
}

func (s *store) create(m Meta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := m
	s.metas[m.ID] = &cp
	s.events[m.ID] = []Event{}
	s.dirty[indexKey] = true
	s.dirty[m.ID] = true
	s.schedule(flushDelay)
}

func (s *store) remove(id string) {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	delete(s.metas, id)
	delete(s.events, id)
	delete(s.dirty, id)
	s.gone[id] = true
	s.dirty[indexKey] = true
	s.schedule(flushDelay)
	s.mu.Unlock()
	_ = os.Remove(s.path(id))
}

func (s *store) path(id string) string {
	if id == indexKey {
		return filepath.Join(s.dir, "index.json")
	}
	return filepath.Join(s.dir, "conversations", id+".json")
}

func (s *store) transcript(id string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.loadLocked(id)...)
}

func (s *store) loadLocked(id string) []Event {
	if ev, ok := s.events[id]; ok {
		return ev
	}
	var ev []Event
	if b, err := os.ReadFile(s.path(id)); err == nil {
		_ = json.Unmarshal(b, &ev)
	}
	if ev == nil {
		ev = []Event{}
	}
	s.events[id] = ev
	return ev
}

// append adds events to a transcript, folding streaming noise into the
// entries it updates (see compact).
func (s *store) append(id string, events ...Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.metas[id]; !ok {
		return
	}
	ev := s.loadLocked(id)
	for _, e := range events {
		ev = compact(ev, e)
	}
	s.events[id] = ev
	s.dirty[id] = true
	s.schedule(flushDelay)
}

func (s *store) schedule(delay time.Duration) {
	if s.timer != nil {
		return
	}
	s.timerGeneration++
	generation := s.timerGeneration
	s.timer = time.AfterFunc(delay, func() { s.flushPending(generation) })
}

func (s *store) flush() {
	s.flushPending(0)
}

func (s *store) flushPending(generation uint64) {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	if generation != 0 && (s.timer == nil || generation != s.timerGeneration) {
		s.mu.Unlock()
		return
	}
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	jobs := make(map[string][]byte, len(s.dirty))
	failed := make(map[string]bool)
	var failures []error
	for id := range s.dirty {
		var data []byte
		var err error
		if id == indexKey {
			data, err = s.marshalIndexLocked()
		} else {
			data, err = json.Marshal(s.events[id])
		}
		if err != nil {
			failed[id] = true
			failures = append(failures, fmt.Errorf("encode %s: %w", s.path(id), err))
			continue
		}
		jobs[id] = data
	}
	clear(s.dirty)
	s.mu.Unlock()
	for id, data := range jobs {
		path := s.path(id)
		if err := writeAtomic(path, data); err != nil {
			failed[id] = true
			failures = append(failures, fmt.Errorf("write %s: %w", path, err))
		}
	}
	if len(failed) == 0 {
		return
	}
	s.mu.Lock()
	for id := range failed {
		s.dirty[id] = true
	}
	s.schedule(flushRetryDelay)
	s.mu.Unlock()
	logf("保存会话失败，将重试: %v", errors.Join(failures...))
}

func (s *store) marshalIndexLocked() ([]byte, error) {
	// Another instance (or an older one still shutting down) may have
	// written conversations this process never saw: keep them.
	if data, err := os.ReadFile(s.path(indexKey)); err == nil {
		var disk []*Meta
		if json.Unmarshal(data, &disk) == nil {
			for _, meta := range disk {
				if meta == nil || meta.ID == "" || s.gone[meta.ID] {
					continue
				}
				if _, known := s.metas[meta.ID]; !known {
					meta.Status, meta.Pending = "idle", 0
					s.metas[meta.ID] = meta
				}
			}
		}
	}
	list := make([]*Meta, 0, len(s.metas))
	for _, meta := range s.metas {
		list = append(list, meta)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	return json.MarshalIndent(list, "", " ")
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// compact appends e to a transcript, merging it into what it updates within
// the current turn: adjacent text deltas join, tool call updates fold into
// their call, a plan replaces the previous plan, a resolved permission marks
// its request. Session-level state (modes, commands, config) isn't kept.
func compact(ev []Event, e Event) []Event {
	t, _ := e["type"].(string)
	switch t {
	case "mode_update", "commands_update", "config_update", "raw", "usage":
		return ev
	case "message_delta", "thought_delta":
		if n := len(ev); n > 0 {
			if last := ev[n-1]; last["type"] == t {
				a, _ := last["text"].(string)
				b, _ := e["text"].(string)
				merged := cloneEvent(last)
				merged["text"] = a + b
				ev[n-1] = merged
				return ev
			}
		}
	case "tool_call_update", "tool_call":
		call, _ := e["toolCall"].(map[string]any)
		id, _ := call["toolCallId"].(string)
		for i := len(ev) - 1; i >= 0; i-- {
			if ev[i]["type"] == "user_message" {
				break
			}
			if ev[i]["type"] != "tool_call" {
				continue
			}
			prev, _ := ev[i]["toolCall"].(map[string]any)
			if prev["toolCallId"] != id {
				continue
			}
			merged := map[string]any{}
			for k, v := range prev {
				merged[k] = v
			}
			for k, v := range call {
				merged[k] = v
			}
			next := cloneEvent(ev[i])
			next["toolCall"] = merged
			ev[i] = next
			return ev
		}
		if t == "tool_call_update" {
			next := cloneEvent(e)
			next["type"] = "tool_call"
			return append(ev, next)
		}
	case "plan":
		for i := len(ev) - 1; i >= 0; i-- {
			if ev[i]["type"] == "user_message" {
				break
			}
			if ev[i]["type"] == "plan" {
				ev[i] = e
				return ev
			}
		}
	case "permission_resolved":
		for i := len(ev) - 1; i >= 0; i-- {
			if ev[i]["type"] == "permission_request" && ev[i]["requestId"] == e["requestId"] {
				next := cloneEvent(ev[i])
				next["resolved"] = map[string]any{"optionId": e["optionId"], "auto": e["auto"]}
				ev[i] = next
				return ev
			}
		}
		return ev
	}
	return append(ev, e)
}

func cloneEvent(e Event) Event {
	out := make(Event, len(e))
	for k, v := range e {
		out[k] = v
	}
	return out
}

// settings.json

func loadSettings(dir string) Settings {
	s := Settings{
		Theme:          "system",
		SendKey:        "modEnter",
		NotifyNeedYou:  true,
		NotifyTurnDone: true,
		NotifySound:    true,
		Agents:         map[string]AgentOverride{},
	}
	if b, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Agents == nil {
		s.Agents = map[string]AgentOverride{}
	}
	if s.Theme == "" {
		s.Theme = "system"
	}
	if s.SendKey == "" {
		s.SendKey = "modEnter"
	}
	return s
}

func saveSettings(dir string, s Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "settings.json"), b)
}
