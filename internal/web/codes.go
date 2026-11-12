package web

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// OneTimeCodes issues short pairing codes for phones and tablets: six digits,
// valid for a limited time, usable once, and revoked after a few wrong guesses.
type OneTimeCodes struct {
	mu      sync.Mutex
	code    string
	expires time.Time
	fails   int
	now     func() time.Time
}

const (
	CodeLifetime = 10 * time.Minute
	codeMaxFails = 5
)

func (c *OneTimeCodes) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// New replaces any current code with a fresh one.
func (c *OneTimeCodes) New() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.code, c.expires, c.fails = fmt.Sprintf("%06d", n.Int64()), c.clock().Add(CodeLifetime), 0
	return c.code, nil
}

// Current returns the active code and its expiry, or "" if none.
func (c *OneTimeCodes) Current() (string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.code == "" || !c.clock().Before(c.expires) {
		c.code = ""
		return "", time.Time{}
	}
	return c.code, c.expires
}

// Take consumes the code if it matches; wrong guesses count toward revocation.
func (c *OneTimeCodes) Take(guess string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.code == "" || !c.clock().Before(c.expires) || len(guess) != len(c.code) {
		if c.code != "" && len(guess) == 6 {
			c.fail()
		}
		return false
	}
	if subtle.ConstantTimeCompare([]byte(guess), []byte(c.code)) != 1 {
		c.fail()
		return false
	}
	c.code = ""
	return true
}

func (c *OneTimeCodes) fail() {
	if c.fails++; c.fails >= codeMaxFails {
		c.code = ""
	}
}
