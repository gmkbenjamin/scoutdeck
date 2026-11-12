// Package events keeps the alert timeline (motion, people and pets seen by
// the camera, obstacles, patrols) with a snapshot for each, and detects
// motion in camera frames on the Mac.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Box is a detection rectangle in normalised image coordinates (0..1).
type Box struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
}

// Event is one entry in the timeline.
type Event struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"` // motion, person, cat, dog, patrol, dock, obstacle, info
	Text     string    `json:"text"`
	At       time.Time `json:"at"`
	Score    float64   `json:"score,omitempty"`
	Box      *Box      `json:"box,omitempty"`
	Snapshot bool      `json:"snapshot"` // a JPEG is stored with the event
}

// Log is a bounded, persistent event timeline. Snapshots are stored next to
// an index file in Dir; only the newest Max events are kept.
type Log struct {
	Dir string
	Max int

	mu     sync.Mutex
	events []Event
	subs   map[chan Event]struct{}
	loaded bool
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ValidID reports whether id has the shape of an event ID, so it can be used
// in a file name.
func ValidID(id string) bool { return idPattern.MatchString(id) }

func (l *Log) max() int {
	if l.Max <= 0 {
		return 200
	}
	return l.Max
}

func (l *Log) load() {
	if l.loaded {
		return
	}
	l.loaded = true
	if l.Dir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(l.Dir, "events.json"))
	if err != nil {
		return
	}
	var list []Event
	if json.Unmarshal(data, &list) == nil {
		for _, e := range list {
			if ValidID(e.ID) {
				l.events = append(l.events, e)
			}
		}
	}
}

// A nil *Log accepts and discards events, so tests need no setup.

// Add records an event with an optional JPEG snapshot and returns it.
func (l *Log) Add(kind, text string, jpeg []byte, score float64, box *Box) (Event, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Event{}, err
	}
	e := Event{ID: hex.EncodeToString(b[:]), Kind: kind, Text: text, At: time.Now(), Score: score, Box: box}
	if l == nil {
		return e, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	var err error
	if l.Dir != "" {
		if err = os.MkdirAll(l.Dir, 0o700); err == nil && len(jpeg) > 0 {
			if err = os.WriteFile(filepath.Join(l.Dir, e.ID+".jpg"), jpeg, 0o600); err == nil {
				e.Snapshot = true
			}
		}
	}
	l.events = append(l.events, e)
	if extra := len(l.events) - l.max(); extra > 0 {
		for _, old := range l.events[:extra] {
			if old.Snapshot && l.Dir != "" {
				_ = os.Remove(filepath.Join(l.Dir, old.ID+".jpg"))
			}
		}
		l.events = append([]Event(nil), l.events[extra:]...)
	}
	if perr := l.persist(); err == nil {
		err = perr
	}
	for ch := range l.subs {
		select {
		case ch <- e:
		default:
		}
	}
	return e, err
}

func (l *Log) persist() error {
	if l.Dir == "" {
		return nil
	}
	data, err := json.MarshalIndent(l.events, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(l.Dir, "events.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(l.Dir, "events.json"))
}

// List returns events newest first, optionally only those after since.
func (l *Log) List(limit int, since time.Time) []Event {
	if l == nil {
		return []Event{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	out := make([]Event, 0, len(l.events))
	for i := len(l.events) - 1; i >= 0; i-- {
		if !l.events[i].At.After(since) {
			break
		}
		out = append(out, l.events[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Snapshot returns the JPEG stored with an event.
func (l *Log) Snapshot(id string) ([]byte, error) {
	if l == nil || !ValidID(id) || l.Dir == "" {
		return nil, fmt.Errorf("no such event")
	}
	return os.ReadFile(filepath.Join(l.Dir, id+".jpg"))
}

// Clear deletes every event and snapshot.
func (l *Log) Clear() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load()
	for _, e := range l.events {
		if e.Snapshot && l.Dir != "" {
			_ = os.Remove(filepath.Join(l.Dir, e.ID+".jpg"))
		}
	}
	l.events = nil
	return l.persist()
}

// Subscribe delivers new events until the returned function is called.
func (l *Log) Subscribe() (<-chan Event, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.subs == nil {
		l.subs = map[chan Event]struct{}{}
	}
	ch := make(chan Event, 16)
	l.subs[ch] = struct{}{}
	return ch, func() {
		l.mu.Lock()
		delete(l.subs, ch)
		l.mu.Unlock()
	}
}

// Counts summarises events by kind since a time, e.g. for "3 motion alerts today".
func (l *Log) Counts(since time.Time) map[string]int {
	out := map[string]int{}
	for _, e := range l.List(0, since) {
		out[e.Kind]++
	}
	return out
}

// Label is a human name for a detection class.
func Label(kind string) string {
	switch kind {
	case "person":
		return "Person"
	case "cat":
		return "Cat"
	case "dog":
		return "Dog"
	case "motion":
		return "Motion"
	}
	if kind == "" {
		return "Event"
	}
	return strings.ToUpper(kind[:1]) + kind[1:]
}

// Kinds returns the distinct kinds in a list, sorted.
func Kinds(list []Event) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range list {
		if !seen[e.Kind] {
			seen[e.Kind] = true
			out = append(out, e.Kind)
		}
	}
	sort.Strings(out)
	return out
}
