package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
	a := &App{cfg: Config{Members: []Member{{Name: "hermes-atlas"}}}, state: s, telegram: tg}
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
	ups := 0
	posts := 0
	statuses := 0
	typings := 0
	replies := []string{}
	sessionID := ""
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer api-secret-0123456789abcdef" {
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
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": "/hermes-atlas", "State": map[string]any{"Running": running}, "Config": map[string]any{"Env": []string{"API_SERVER_PORT=" + apiPort}}, "HostConfig": map[string]any{"NetworkMode": "host"}})
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

	// Compose fake: the member starts asleep; `up` wakes it. API env is served
	// from a persistent data/.env whose key the bridge must adopt (not rewrite).
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dataDir, ".env")
	if err := os.WriteFile(envPath, []byte("API_SERVER_KEY=api-secret-0123456789abcdef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	compose := newCompose()
	compose.exec = func(ctx context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		rest := args
		if len(rest) > 0 && rest[0] == "-f" {
			rest = rest[4:] // skip -f <file> --project-directory <dir>
		}
		switch {
		case rest[0] == "ps":
			state := "created"
			if running {
				state = "running"
			}
			return []byte(fmt.Sprintf(`{"Name":"hermes-atlas","State":%q,"Service":"hermes-atlas"}`+"\n", state)), nil
		case rest[0] == "up":
			running = true
			ups++
			return nil, nil
		case rest[0] == "down":
			running = false
			return nil, nil
		case rest[0] == "config":
			return []byte(fmt.Sprintf(`{"services":{"hermes-atlas":{"volumes":[{"type":"bind","source":%q,"target":"/opt/data"}]}}}`, dataDir)), nil
		default:
			return nil, fmt.Errorf("unexpected compose args %v", rest)
		}
	}

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
	a := &App{cfg: cfg, state: s, telegram: tg, docker: newDocker(socket), compose: compose, dirs: map[string]string{"hermes-atlas": dir}, lifecycle: map[string]*sync.Mutex{"hermes-atlas": &sync.Mutex{}}, busy: map[string]*atomic.Bool{"hermes-atlas": &atomic.Bool{}}, issues: map[string]string{}}
	p := s.first("hermes-atlas")
	if err := a.processMessage(context.Background(), "hermes-atlas", p); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ups != 1 || posts != 1 || statuses < 2 || typings < 1 || sessionID == "" {
		t.Fatalf("up=%d posts=%d statuses=%d typing=%d session=%q", ups, posts, statuses, typings, sessionID)
	}
	// The general service messages and final reply are all sent via sendMessage.
	found := false
	for _, r := range replies {
		if r == "Hello from Hermes" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected Hermes reply in replies=%v", replies)
	}
	if s.first("hermes-atlas") != nil {
		t.Fatal("pending item not removed")
	}
	// The bridge must adopt the existing key, not rotate it.
	if got := s.apiKey("hermes-atlas"); got != "api-secret-0123456789abcdef" {
		t.Fatalf("bridge did not adopt the member's key: %q", got)
	}
	if env := readEnvValue(t, envPath, "API_SERVER_KEY"); env != "api-secret-0123456789abcdef" {
		t.Fatalf("member .env key was rewritten: %q", env)
	}
	if host := readEnvValue(t, envPath, "API_SERVER_HOST"); host != "0.0.0.0" {
		t.Fatalf("member .env host wrong: %q", host)
	}
}

func TestOnlyAllowedUsersAndTopicsRoute(t *testing.T) {
	s, _ := openStore(t.TempDir(), -1009)
	_ = s.setTopic("hermes-atlas", Topic{ID: 777, Title: "Atlas"})
	app := &App{cfg: Config{GroupID: -1009, Allowed: map[int64]bool{42: true}}, state: s, active: []string{"hermes-atlas"}}
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
	app := &App{cfg: Config{GroupID: -1009, Allowed: map[int64]bool{42: true}}, state: s, telegram: tg, active: []string{"hermes-atlas"}, all: []string{"hermes-atlas"}, issues: map[string]string{}}
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

func TestStatusReportsSkippedMemberIssues(t *testing.T) {
	s, _ := openStore(t.TempDir(), -1009)
	app := &App{state: s, all: []string{"hermes-atlas", "hermes-ghost"}, active: []string{"hermes-atlas"}, issues: map[string]string{}}
	app.setIssue("hermes-ghost", "no compose file in project directory")
	msg := app.statusMessage()
	if !strings.Contains(msg, "hermes-ghost: no compose file in project directory") {
		t.Fatalf("status missing skipped member issue: %q", msg)
	}
	started := app.serviceStartMessage([]string{"hermes-atlas"}, time.Unix(0, 0))
	if !strings.Contains(started, "Serving 1/2 members") || !strings.Contains(started, "hermes-ghost: no compose file in project directory") {
		t.Fatalf("startup banner wrong: %q", started)
	}
}

func TestIdleStopsOnlyConfiguredContainers(t *testing.T) {
	var mu sync.Mutex
	downs := 0
	running := true
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
		default:
			http.Error(w, "unknown container", 404)
		}
	}))
	dockerServer.Listener = ln
	dockerServer.Start()
	defer dockerServer.Close()
	compose := newCompose()
	compose.exec = func(ctx context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		rest := args
		if len(rest) > 0 && rest[0] == "-f" {
			rest = rest[4:] // skip -f <file> --project-directory <dir>
		}
		switch {
		case rest[0] == "ps":
			state := "created"
			if running {
				state = "running"
			}
			return []byte(fmt.Sprintf(`{"Name":"hermes-atlas","State":%q}`+"\n", state)), nil
		case rest[0] == "down":
			downs++
			running = false
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected compose args %v", rest)
		}
	}
	st, _ := openStore(t.TempDir(), -1009)
	if err := st.touch("hermes-atlas"); err != nil {
		t.Fatal(err)
	}
	idleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(idleDir, "docker-compose.yml"), []byte("services: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{IdleTimeout: 10 * time.Second}, state: st, docker: newDocker(socket), compose: compose, dirs: map[string]string{"hermes-atlas": idleDir}, active: []string{"hermes-atlas"}, busy: map[string]*atomic.Bool{"hermes-atlas": &atomic.Bool{}}, lifecycle: map[string]*sync.Mutex{"hermes-atlas": &sync.Mutex{}}}
	app.stopIdle(context.Background())
	if downs != 0 {
		t.Fatal("stopped container before idle")
	}
	st.mu.Lock()
	st.s.LastActivity["hermes-atlas"] = time.Now().Add(-time.Minute)
	st.mu.Unlock()
	app.busy["hermes-atlas"].Store(true)
	app.stopIdle(context.Background())
	if downs != 0 {
		t.Fatal("stopped busy container")
	}
	app.busy["hermes-atlas"].Store(false)
	app.stopIdle(context.Background())
	if downs != 1 {
		t.Fatalf("expected exactly one down, got %d", downs)
	}
}

// readEnvValue reads KEY=VALUE from a .env file path.
func readEnvValue(t *testing.T, path, key string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if v := envValue(string(b), key); v != "" {
		return v
	}
	return ""
}
