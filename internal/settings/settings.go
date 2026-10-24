// Package settings holds the runtime settings that the admin panel changes.
// They live in the SQLite settings table and are cached in memory.
package settings

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/amwolff/yamp/internal/privacy"
	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/store/db"
)

// Setting keys.
const (
	KeySalt         = "salt"
	KeyScanInterval = "scan_interval_minutes"
	KeyWorkers      = "workers"
)

var defaults = map[string]string{
	KeyScanInterval: "0",
	KeyWorkers:      "1",
}

// Settings is a cached view of the settings table.
type Settings struct {
	st    *store.Store
	mu    sync.RWMutex
	cache map[string]string
}

// Load reads all settings and creates a random salt on the first start.
func Load(ctx context.Context, st *store.Store) (*Settings, error) {
	s := &Settings{st: st, cache: make(map[string]string)}
	for _, k := range []string{KeySalt, KeyScanInterval, KeyWorkers} {
		v, err := st.GetSetting(ctx, k)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			v = defaults[k]
			if k == KeySalt {
				v = privacy.NewSalt()
				if err = s.Set(ctx, k, v); err != nil {
					return nil, err
				}
			}
		case err != nil:
			return nil, err
		}
		s.cache[k] = v
	}
	return s, nil
}

// Set stores a setting.
func (s *Settings) Set(ctx context.Context, key, value string) error {
	if err := s.st.SetSetting(ctx, db.SetSettingParams{Key: key, Value: value}); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache[key] = value
	s.mu.Unlock()
	return nil
}

// SetCached updates the cache after a caller stored a setting inside its own
// transaction.
func (s *Settings) SetCached(key, value string) {
	s.mu.Lock()
	s.cache[key] = value
	s.mu.Unlock()
}

func (s *Settings) get(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cache[key]
}

func (s *Settings) getInt(key string) int {
	n, err := strconv.Atoi(s.get(key))
	if err != nil {
		n, _ = strconv.Atoi(defaults[key])
	}
	return n
}

// Salt returns the salt for public IDs.
func (s *Settings) Salt() string { return s.get(KeySalt) }

// ScanInterval returns the automatic scan interval, or 0 if it is off.
func (s *Settings) ScanInterval() time.Duration {
	return time.Duration(max(s.getInt(KeyScanInterval), 0)) * time.Minute
}

// Workers returns the number of concurrent transcode jobs.
func (s *Settings) Workers() int {
	return min(max(s.getInt(KeyWorkers), 1), 8)
}
