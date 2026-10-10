package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Topic struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}
type Pending struct {
	UpdateID  int64  `json:"update_id"`
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
	RunID     string `json:"run_id,omitempty"`
	Reply     string `json:"reply,omitempty"`
	Sent      int    `json:"sent,omitempty"`
	Attempts  int    `json:"attempts,omitempty"`
}
type State struct {
	GroupID      int64                 `json:"group_id"`
	Offset       int64                 `json:"offset"`
	Topics       map[string]Topic      `json:"topics"`
	Pending      map[string][]*Pending `json:"pending"`
	LastActivity map[string]time.Time  `json:"last_activity"`
	APIKeys      map[string]string     `json:"api_keys,omitempty"`
}
type Store struct {
	mu   sync.Mutex
	path string
	s    State
}

func openStore(dir string, groupID int64) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	st := &Store{path: filepath.Join(dir, "state.json"), s: State{GroupID: groupID, Topics: map[string]Topic{}, Pending: map[string][]*Pending{}, LastActivity: map[string]time.Time{}, APIKeys: map[string]string{}}}
	data, err := os.ReadFile(st.path)
	if err == nil {
		if err = json.Unmarshal(data, &st.s); err != nil {
			return nil, fmt.Errorf("corrupt state file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if st.s.GroupID != groupID {
		// The configured Telegram group changed: archive the old state and start fresh
		// so the bridge self-heals instead of crash-looping.
		archive := fmt.Sprintf("%s.bak.%s", st.path, time.Now().UTC().Format("20060102_150405"))
		if err := os.Rename(st.path, archive); err != nil {
			return nil, fmt.Errorf("Telegram group ID changed (%d -> %d) and cannot archive %s: %w", st.s.GroupID, groupID, st.path, err)
		}
		log.Printf("state: Telegram group ID changed (%d -> %d); archived old state to %s", st.s.GroupID, groupID, archive)
		st.s = State{GroupID: groupID, Topics: map[string]Topic{}, Pending: map[string][]*Pending{}, LastActivity: map[string]time.Time{}, APIKeys: map[string]string{}}
	}
	if st.s.Topics == nil {
		st.s.Topics = map[string]Topic{}
	}
	if st.s.Pending == nil {
		st.s.Pending = map[string][]*Pending{}
	}
	if st.s.LastActivity == nil {
		st.s.LastActivity = map[string]time.Time{}
	}
	if st.s.APIKeys == nil {
		st.s.APIKeys = map[string]string{}
	}
	return st, nil
}

// Must hold st.mu. Uses atomic replacement so a crash cannot truncate the state file.
func (st *Store) save() error {
	data, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(st.path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), st.path); err != nil {
		return err
	}
	// Directory sync helps state survive unexpected host power loss.
	if dir, e := os.Open(filepath.Dir(st.path)); e == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
func (st *Store) topic(name string) (Topic, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	v, ok := st.s.Topics[name]
	return v, ok
}
func (st *Store) setTopic(name string, t Topic) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.Topics[name] = t
	return st.save()
}

// apiKey returns the persisted Hermes API key for a member, if any.
func (st *Store) apiKey(name string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s.APIKeys[name]
}

// setAPIKey persists the key the bridge wrote into the member's .env so the same
// key is reused across restarts even if the member's .env is later regenerated.
func (st *Store) setAPIKey(name, key string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.APIKeys[name] == key {
		return nil
	}
	st.s.APIKeys[name] = key
	return st.save()
}
func (st *Store) offset() int64 { st.mu.Lock(); defer st.mu.Unlock(); return st.s.Offset }
func (st *Store) ingest(u Update, agent string) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if u.UpdateID < st.s.Offset {
		return false, nil
	}
	if agent != "" && u.Message != nil {
		st.s.Pending[agent] = append(st.s.Pending[agent], &Pending{UpdateID: u.UpdateID, MessageID: u.Message.MessageID, Text: u.Message.Text})
	}
	st.s.Offset = u.UpdateID + 1
	return agent != "", st.save()
}
func (st *Store) first(agent string) *Pending {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.s.Pending[agent]) == 0 {
		return nil
	}
	p := *st.s.Pending[agent][0]
	return &p
}
func (st *Store) updatePending(agent string, p *Pending) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.s.Pending[agent]) == 0 || st.s.Pending[agent][0].UpdateID != p.UpdateID {
		return fmt.Errorf("pending item moved for %s", agent)
	}
	cp := *p
	st.s.Pending[agent][0] = &cp
	return st.save()
}
func (st *Store) finish(agent string, id int64) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.s.Pending[agent]) == 0 || st.s.Pending[agent][0].UpdateID != id {
		return fmt.Errorf("unexpected pending message")
	}
	st.s.Pending[agent] = st.s.Pending[agent][1:]
	st.s.LastActivity[agent] = time.Now()
	return st.save()
}
func (st *Store) touch(agent string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.s.LastActivity[agent] = time.Now()
	return st.save()
}
func (st *Store) idle(agent string, now time.Time, timeout time.Duration) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.s.Pending[agent]) > 0 {
		return false
	}
	last := st.s.LastActivity[agent]
	return !last.IsZero() && now.Sub(last) > timeout
}
func (st *Store) initActivity(agents []string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, a := range agents {
		if st.s.LastActivity[a].IsZero() {
			st.s.LastActivity[a] = time.Now()
		}
	}
	return st.save()
}
