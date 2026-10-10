package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSplitTelegramUTF16(t *testing.T) {
	parts := splitTelegram(strings.Repeat("🦌", 4000))
	if len(parts) != 3 {
		t.Fatalf("wanted 3 parts; got %d", len(parts))
	}
	if strings.Join(parts, "") != strings.Repeat("🦌", 4000) {
		t.Fatal("split modified text")
	}
	if topicName("hermes-janus") != "Janus" {
		t.Fatal("unexpected automatic topic name")
	}
	a, b := sessionIDs(-100123, "hermes-janus")
	c, _ := sessionIDs(-100123, "hermes-janus")
	if a != c || b == "" {
		t.Fatal("session not deterministic")
	}
}

func TestDurableStateQueueAndOffset(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir, -1009)
	if err != nil {
		t.Fatal(err)
	}
	var u Update
	if err := json.Unmarshal([]byte(`{"update_id":123,"message":{"message_id":42,"text":"hello"}}`), &u); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ingest(u, "atlas"); err != nil {
		t.Fatal(err)
	}
	if s.offset() != 124 {
		t.Fatalf("offset is %d", s.offset())
	}
	reopened, err := openStore(dir, -1009)
	if err != nil {
		t.Fatal(err)
	}
	if p := reopened.first("atlas"); p == nil || p.Text != "hello" {
		t.Fatal("queue missing after restart")
	}
	if consumed, err := reopened.ingest(u, "atlas"); err != nil || consumed {
		t.Fatal("duplicate update accepted")
	}
	if err := reopened.finish("atlas", 123); err != nil {
		t.Fatal(err)
	}
	if reopened.first("atlas") != nil {
		t.Fatal("queue not drained")
	}
	// A changed group ID must self-heal: archive the old file and start empty.
	changed, err := openStore(dir, -1008)
	if err != nil {
		t.Fatalf("group ID change should recover, got %v", err)
	}
	if changed.offset() != 0 || changed.first("atlas") != nil {
		t.Fatal("expected fresh state after group ID change")
	}
	if backups, _ := filepath.Glob(filepath.Join(dir, "state.json.bak.*")); len(backups) != 1 {
		t.Fatalf("expected exactly one archived state file, got %v", backups)
	}
}

func TestTelegramAutoTopics(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/botTEST/")
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		var result any = true
		if method == "createForumTopic" {
			result = map[string]any{"message_thread_id": 555, "name": "Atlas"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer server.Close()
	s, _ := openStore(t.TempDir(), -1009)
	tg := newTelegram("TEST", -1009)
	tg.base = server.URL + "/botTEST/"
	a := &App{cfg: Config{Containers: []string{"hermes-atlas"}}, state: s, telegram: tg}
	if err := a.ensureTopic(context.Background(), "hermes-atlas", false); err != nil {
		t.Fatal(err)
	}
	if err := s.setTopic("hermes-atlas", Topic{ID: 555, Title: "Old Name"}); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureTopic(context.Background(), "hermes-atlas", false); err != nil {
		t.Fatal(err)
	}
	topic, ok := s.topic("hermes-atlas")
	if !ok || topic.ID != 555 || topic.Title != "Atlas" {
		t.Fatalf("bad mapping %+v", topic)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 || methods[0] != "createForumTopic" || methods[1] != "editForumTopic" {
		t.Fatalf("unexpected methods: %v", methods)
	}
}

func TestSleepingContainerWakeRunTypingAndReply(t *testing.T) {
	var mu sync.Mutex
	running := false
	starts := 0
	posts := 0
	statuses := 0
	typings := 0
	replies := []string{}
	sessionID := ""
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer api-secret" {
			http.Error(w, "unauthorised", 401)
			return
		}
		switch {
		case r.URL.Path == "/health":
			w.WriteHeader(200)
		case r.URL.Path == "/v1/runs" && r.Method == "POST":
			mu.Lock()
			posts++
			sessionID = r.Header.Get("X-Hermes-Session-Id")
			mu.Unlock()
			w.WriteHeader(202)
			fmt.Fprint(w, `{"run_id":"run123"}`)
		case r.URL.Path == "/v1/runs/run123":
			mu.Lock()
			statuses++
			count := statuses
			mu.Unlock()
			if count < 2 {
				fmt.Fprint(w, `{"status":"running"}`)
			} else {
				fmt.Fprint(w, `{"status":"completed","output":"Hello from Hermes"}`)
			}
		default:
			http.Error(w, "unexpected path", 404)
		}
	}))
	defer api.Close()
	apiPort := strings.TrimPrefix(api.URL, "http://127.0.0.1:")

	socket := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	dockerServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/containers/hermes-atlas/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": "/hermes-atlas", "State": map[string]any{"Running": running}, "Config": map[string]any{"Env": []string{"API_SERVER_KEY=api-secret", "API_SERVER_PORT=" + apiPort}}, "HostConfig": map[string]any{"NetworkMode": "host"}})
		case "/containers/hermes-atlas/start":
			running = true
			starts++
			w.WriteHeader(204)
		default:
			http.Error(w, "invalid Docker operation", 404)
		}
	}))
	dockerServer.Listener = ln
	dockerServer.Start()
	defer dockerServer.Close()

	telegramServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/botTEST/")
		mu.Lock()
		defer mu.Unlock()
		if method == "sendChatAction" {
			typings++
		}
		if method == "sendMessage" {
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			replies = append(replies, body.Text)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	}))
	defer telegramServer.Close()

	s, _ := openStore(t.TempDir(), -1009)
	if err := s.setTopic("hermes-atlas", Topic{ID: 123, Title: "Atlas"}); err != nil {
		t.Fatal(err)
	}
	var update Update
	if err := json.Unmarshal([]byte(`{"update_id":11,"message":{"message_id":44,"text":"Hello"}}`), &update); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ingest(update, "hermes-atlas"); err != nil {
		t.Fatal(err)
	}
	tg := newTelegram("TEST", -1009)
	tg.base = telegramServer.URL + "/botTEST/"
	cfg := Config{GroupID: -1009, DefaultPort: 8642, StartTimeout: 5 * time.Second, RunTimeout: time.Minute}
	a := &App{cfg: cfg, state: s, telegram: tg, docker: newDocker(socket), lifecycle: map[string]*sync.Mutex{"hermes-atlas": &sync.Mutex{}}, busy: map[string]*atomic.Bool{"hermes-atlas": &atomic.Bool{}}, issues: map[string]string{}}
	p := s.first("hermes-atlas")
	if err := a.processMessage(context.Background(), "hermes-atlas", p); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 || posts != 1 || statuses < 2 || typings < 1 || len(replies) != 1 || replies[0] != "Hello from Hermes" || sessionID == "" {
		t.Fatalf("start=%d posts=%d statuses=%d typing=%d replies=%v session=%q", starts, posts, statuses, typings, replies, sessionID)
	}
	if s.first("hermes-atlas") != nil {
		t.Fatal("pending item not removed")
	}
}

func TestOnlyAllowedUsersAndTopicsRoute(t *testing.T) {
	s, _ := openStore(t.TempDir(), -1009)
	_ = s.setTopic("hermes-atlas", Topic{ID: 777, Title: "Atlas"})
	app := &App{cfg: Config{GroupID: -1009, Allowed: map[int64]bool{42: true}, Containers: []string{"hermes-atlas"}}, state: s}
	mk := func(chatID, id int64, thread int, bot bool) Update {
		var u Update
		b, _ := json.Marshal(map[string]any{"update_id": 1, "message": map[string]any{"chat": map[string]any{"id": chatID}, "from": map[string]any{"id": id, "is_bot": bot}, "text": "hi", "message_thread_id": thread}})
		if err := json.Unmarshal(b, &u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	cases := []struct {
		u    Update
		want string
	}{
		{mk(-1009, 42, 777, false), "hermes-atlas"},
		{mk(-1008, 42, 777, false), ""},
		{mk(-1009, 43, 777, false), ""},
		{mk(-1009, 42, 778, false), ""},
		{mk(-1009, 42, 777, true), ""},
	}
	for i, c := range cases {
		if got := app.routeUpdate(c.u); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}
func TestGeneralRoutesCommandsNotAgents(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/botTEST/")
		if method == "sendMessage" {
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			sent = append(sent, body.Text)
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	}))
	defer server.Close()
	s, _ := openStore(t.TempDir(), -1009)
	_ = s.setTopic("hermes-atlas", Topic{ID: 777, Title: "Atlas"})
	tg := newTelegram("TEST", -1009)
	tg.base = server.URL + "/botTEST/"
	app := &App{cfg: Config{GroupID: -1009, Allowed: map[int64]bool{42: true}, Containers: []string{"hermes-atlas"}}, state: s, telegram: tg, all: []string{"hermes-atlas"}, issues: map[string]string{}}
	mk := func(thread int, text string) Update {
		var u Update
		b, _ := json.Marshal(map[string]any{"update_id": 1, "message": map[string]any{"chat": map[string]any{"id": int64(-1009)}, "from": map[string]any{"id": 42, "is_bot": false}, "text": text, "message_thread_id": thread}})
		if err := json.Unmarshal(b, &u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	if got := app.routeUpdate(mk(generalThread, "/status")); got != "" {
		t.Fatalf("General should not route to an agent, got %q", got)
	}
	if got := app.routeUpdate(mk(generalThread, "hello")); got != "" {
		t.Fatalf("General plain text should be ignored, got %q", got)
	}
	if got := app.routeUpdate(mk(777, "hello")); got != "hermes-atlas" {
		t.Fatalf("topic text should route to agent, got %q", got)
	}
	if got := app.routeUpdate(mk(999, "/status")); got != "" {
		t.Fatalf("unknown topic must not route, got %q", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || !strings.Contains(sent[0], "Hermes Fleet Bridge status") {
		t.Fatalf("expected one status reply, got %v", sent)
	}
}

func TestStatusReportsSkippedContainerIssues(t *testing.T) {
	s, _ := openStore(t.TempDir(), -1009)
	app := &App{cfg: Config{Containers: []string{"hermes-atlas"}}, state: s, all: []string{"hermes-atlas", "hermes-ghost"}, issues: map[string]string{}}
	app.setIssue("hermes-ghost", "container not found")
	msg := app.statusMessage()
	if !strings.Contains(msg, "hermes-ghost: container not found") {
		t.Fatalf("status missing skipped container issue: %q", msg)
	}
	started := app.serviceStartMessage([]string{"hermes-atlas"}, time.Unix(0, 0))
	if !strings.Contains(started, "Serving 1/2 containers") || !strings.Contains(started, "hermes-ghost: container not found") {
		t.Fatalf("startup banner wrong: %q", started)
	}
}

func TestIdleStopsOnlyConfiguredContainers(t *testing.T) {
	var mu sync.Mutex
	stopCalls := 0
	socket := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	dockerServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/containers/hermes-atlas/json":
			fmt.Fprint(w, `{"State":{"Running":true}}`)
		case "/containers/hermes-atlas/stop":
			stopCalls++
			w.WriteHeader(204)
		default:
			http.Error(w, "unknown container", 404)
		}
	}))
	dockerServer.Listener = ln
	dockerServer.Start()
	defer dockerServer.Close()
	st, _ := openStore(t.TempDir(), -1009)
	if err := st.touch("hermes-atlas"); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{Containers: []string{"hermes-atlas"}, IdleTimeout: 10 * time.Second}, state: st, docker: newDocker(socket), busy: map[string]*atomic.Bool{"hermes-atlas": &atomic.Bool{}}, lifecycle: map[string]*sync.Mutex{"hermes-atlas": &sync.Mutex{}}}
	app.stopIdle(context.Background())
	if stopCalls != 0 {
		t.Fatal("stopped container before idle")
	}
	st.mu.Lock()
	st.s.LastActivity["hermes-atlas"] = time.Now().Add(-time.Minute)
	st.mu.Unlock()
	app.busy["hermes-atlas"].Store(true)
	app.stopIdle(context.Background())
	if stopCalls != 0 {
		t.Fatal("stopped busy container")
	}
	app.busy["hermes-atlas"].Store(false)
	app.stopIdle(context.Background())
	if stopCalls != 1 {
		t.Fatalf("expected exactly one stopped container, got %d", stopCalls)
	}
}
