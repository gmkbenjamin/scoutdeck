package control

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Frame struct {
	Data []byte
	At   time.Time
}

// Media keeps immutable copies so screenshots never consume the render queue.
type Media struct {
	mu    sync.RWMutex
	frame Frame
	audio map[chan []byte]struct{}
}

func (m *Media) PutFrame(data []byte) {
	m.mu.Lock()
	m.frame = Frame{Data: append([]byte(nil), data...), At: time.Now()}
	m.mu.Unlock()
}

func (m *Media) Frame() Frame {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Frame{Data: append([]byte(nil), m.frame.Data...), At: m.frame.At}
}

func (m *Media) Save(dir string) (string, error) {
	f := m.Frame()
	if len(f.Data) == 0 {
		return "", fmt.Errorf("no camera frame available")
	}
	file, err := os.CreateTemp(dir, "scoutdeck-*.jpg")
	if err != nil {
		return "", err
	}
	name := file.Name()
	_, err = file.Write(f.Data)
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return filepath.Clean(name), nil
}

func (m *Media) Audio(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.audio {
		packet := append([]byte(nil), data...)
		select {
		case ch <- packet:
		default:
		}
	}
}

func (m *Media) SubscribeAudio() (<-chan []byte, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.audio == nil {
		m.audio = make(map[chan []byte]struct{})
	}
	ch := make(chan []byte, 32)
	m.audio[ch] = struct{}{}
	return ch, func() {
		m.mu.Lock()
		delete(m.audio, ch)
		m.mu.Unlock()
	}
}
