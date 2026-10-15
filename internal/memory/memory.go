// Package memory keeps what the AI should know about this PC before working
// on it: notes the AI or the owner leave ("ComfyUI lives in D:\ComfyUI"),
// stored in memory.json next to the other agent data.
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// Note is one remembered fact.
type Note struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Text string    `json:"text"`
	Tags []string  `json:"tags,omitempty"`
	By   string    `json:"by,omitempty"` // "panel" when the owner wrote it
}

const (
	maxNotes = 200
	maxText  = 1000
)

// Store is the notes file.
type Store struct {
	path string

	mu    sync.Mutex
	notes []Note
	seq   int
}

// Open loads path (a missing or broken file starts empty).
func Open(path string) *Store {
	s := &Store{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.notes)
	}
	for _, n := range s.notes {
		var k int
		if _, err := fmt.Sscanf(n.ID, "n%d", &k); err == nil && k > s.seq {
			s.seq = k
		}
	}
	return s
}

// Add stores a note; same text again just refreshes it.
func (s *Store) Add(text string, tags []string, by string) (Note, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Note{}, errors.New(`"text" is required`)
	}
	if r := []rune(text); len(r) > maxText {
		text = string(r[:maxText])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = slices.DeleteFunc(s.notes, func(n Note) bool { return n.Text == text })
	s.seq++
	n := Note{ID: fmt.Sprintf("n%d", s.seq), Time: time.Now(), Text: text, Tags: tags, By: by}
	s.notes = append(s.notes, n)
	if len(s.notes) > maxNotes {
		s.notes = s.notes[len(s.notes)-maxNotes:]
	}
	return n, s.save()
}

// Forget deletes a note by id.
func (s *Store) Forget(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.notes)
	s.notes = slices.DeleteFunc(s.notes, func(x Note) bool { return x.ID == id })
	if len(s.notes) == n {
		return fmt.Errorf("no note %q", id)
	}
	return s.save()
}

// Notes returns all notes, oldest first (never nil: [] in JSON).
func (s *Store) Notes() []Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Note{}, s.notes...)
}

func (s *Store) save() error {
	data, err := json.MarshalIndent(s.notes, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
