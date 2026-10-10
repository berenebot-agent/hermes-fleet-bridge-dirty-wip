package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// generalThread is Telegram's fixed message_thread_id for a forum's General topic.
const generalThread = 1

type App struct {
	cfg       Config
	docker    *Docker
	compose   *Compose
	telegram  *Telegram
	state     *Store
	busy      map[string]*atomic.Bool
	lifecycle map[string]*sync.Mutex
	dirs      map[string]string
	all       []string
	active    []string
	issuesMu  sync.Mutex
	issues    map[string]string
}

// dir resolves a member name to its compose project directory. Empty if unknown.
func (a *App) dir(agent string) string { return a.dirs[agent] }

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal("config: ", err)
	}
	st, err := openStore(cfg.DataDir, cfg.GroupID)
	if err != nil {
		log.Fatal("state: ", err)
	}
	a := &App{cfg: cfg, docker: newDocker(cfg.DockerSocket), compose: newCompose(), telegram: newTelegram(cfg.Token, cfg.GroupID), state: st, busy: map[string]*atomic.Bool{}, lifecycle: map[string]*sync.Mutex{}, dirs: map[string]string{}, issues: map[string]string{}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.telegram.deleteWebhook(ctx); err != nil {
		log.Printf("Telegram webhook reset: %v", err)
	}
	started := time.Now().UTC()
	// A member is a compose project directory. Its container is ephemeral, so a
	// missing or stopped container is not a reason to drop it from the fleet; the
	// bridge brings it up on demand. Only a missing compose file/ directory is an
	// unusable configuration.
	var active []Member
	for _, m := range cfg.Members {
		a.busy[m.Name] = &atomic.Bool{}
		a.lifecycle[m.Name] = &sync.Mutex{}
		a.dirs[m.Name] = m.Dir
		a.all = append(a.all, m.Name)
		if err := st.touch(m.Name); err != nil {
			log.Fatal("state write: ", err)
		}
		if _, err := composeFile(m.Dir); err != nil {
			a.setIssue(m.Name, "no compose file in project directory")
			log.Printf("member %s: %v; skipping, reporting in General", m.Name, err)
			continue
		}
		active = append(active, m)
	}
	a.cfg.Members = active
	for _, m := range active {
		a.active = append(a.active, m.Name)
	}
	log.Printf("bridge configured: %d/%d Hermes members, group %d, idle %v; service messages in General", len(active), len(cfg.Members), cfg.GroupID, cfg.IdleTimeout)
	go a.notifyGeneral(a.serviceStartMessage(a.active, started))
	for _, m := range active {
		go a.agentWorker(ctx, m.Name)
	}
	go a.idleMonitor(ctx)
	go a.topicMonitor(ctx)
	a.poll(ctx)
}

func (a *App) topicMonitor(ctx context.Context) {
	a.syncTopics(ctx)
	a.checkMembers(ctx)
	tick := time.NewTicker(1 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.syncTopics(ctx)
			a.checkMembers(ctx)
		}
	}
}

// checkMembers verifies each configured member's project is usable and reports
// issue transitions (appeared, changed, recovered) in General. A stopped or
// absent container is normal and is not an issue.
func (a *App) checkMembers(ctx context.Context) {
	before := a.issueSnapshot()
	for _, name := range a.all {
		if _, err := composeFile(a.dir(name)); err != nil {
			a.setIssue(name, "no compose file in project directory")
			continue
		}
		a.clearIssue(name)
	}
	after := a.issueSnapshot()
	for _, name := range a.all {
		oldMsg, had := before[name]
		newMsg, has := after[name]
		switch {
		case has && (!had || oldMsg != newMsg):
			a.notifyGeneral(fmt.Sprintf("⚠️ Member %s: %s", name, newMsg))
		case had && !has:
			a.notifyGeneral(fmt.Sprintf("✅ Member %s: recovered", name))
		}
	}
}
func (a *App) syncTopics(ctx context.Context) {
	for _, agent := range a.active {
		if err := a.ensureTopic(ctx, agent, false); err != nil {
			log.Printf("topic %s: %v", agent, err)
		}
	}
}
func (a *App) ensureTopic(ctx context.Context, agent string, force bool) error {
	title := topicName(agent)
	if t, ok := a.state.topic(agent); ok && !force {
		if t.Title == title {
			return nil
		}
		err := a.telegram.editTopic(ctx, t.ID, title)
		if err == nil {
			if t.Title != title {
				if err := a.state.setTopic(agent, Topic{ID: t.ID, Title: title}); err != nil {
					return err
				}
				log.Printf("topic renamed: %s (%d)", title, t.ID)
			}
			return nil
		}
		if !isMissingTopic(err) {
			return err
		}
		log.Printf("topic missing for %s; recreating", agent)
	}
	t, err := a.telegram.createTopic(ctx, title)
	if err != nil {
		return err
	}
	if err := a.state.setTopic(agent, t); err != nil {
		return err
	}
	log.Printf("topic mapped: %s -> %s (thread %d)", agent, title, t.ID)
	return nil
}
func isMissingTopic(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "thread not found") || strings.Contains(s, "message_thread_invalid") || strings.Contains(s, "topic_id_invalid") || strings.Contains(s, "topic_deleted")
}
func (a *App) getAgentForThread(thread int) string {
	for _, agent := range a.active {
		if t, ok := a.state.topic(agent); ok && t.ID == thread {
			return agent
		}
	}
	return ""
}

// routeUpdate is fail-closed: no DMs, other groups, bots, unknown users or unknown topics.
// General (thread 1) only accepts the /status and /help commands.
func (a *App) routeUpdate(u Update) string {
	m := u.Message
	if m == nil || m.Chat.ID != a.cfg.GroupID || m.From == nil || m.From.IsBot || !a.cfg.Allowed[m.From.ID] || m.Text == "" {
		return ""
	}
	if m.MessageThreadID <= generalThread {
		go a.handleCommand(m.Text)
		return ""
	}
	return a.getAgentForThread(m.MessageThreadID)
}
func (a *App) handleCommand(text string) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(fields) == 0 {
		return
	}
	switch fields[0] {
	case "/help":
		_ = a.telegram.sendMessage(context.Background(), generalThread, helpMessage())
	case "/status":
		_ = a.telegram.sendMessage(context.Background(), generalThread, a.statusMessage())
	}
}
func helpMessage() string {
	return "Hermes Fleet Bridge\n\n" +
		"Each configured Hermes container has its own topic; send text there to talk to it.\n\n" +
		"General commands:\n" +
		"/status - bridge and container health\n" +
		"/help - this message"
}

// notifyGeneral best-effort posts a service message; failures don't stop the bridge.
func (a *App) notifyGeneral(text string) {
	if a.telegram == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.telegram.sendMessage(ctx, generalThread, text); err != nil {
		log.Printf("General message failed: %v", err)
	}
}
func (a *App) serviceStartMessage(active []string, started time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hermes Fleet Bridge online (%s UTC)\n", started.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "Serving %d/%d members, idle timeout %v", len(active), len(a.all), a.cfg.IdleTimeout)
	for _, n := range active {
		// Get container status for this member
		status := a.memberStatus(n)
		fmt.Fprintf(&b, "\n- %s %s -> %s", status, n, topicName(n))
	}
	if issues := a.issueSnapshot(); len(issues) > 0 {
		b.WriteString("\nIssues:")
		for _, n := range a.all {
			if msg, ok := issues[n]; ok {
				fmt.Fprintf(&b, "\n- %s: %s", n, msg)
			}
		}
	}
	return b.String()
}

// memberStatus returns an emoji indicating the container's running state.
func (a *App) memberStatus(name string) string {
	ctx := context.Background()
	// Try to inspect the container directly first
	if a.docker != nil {
		info, err := a.docker.inspect(ctx, name)
		if err == nil && info.State.Running {
			return "🟢"
		}
	}

	// Fallback to compose status if direct inspection fails
	if a.compose == nil || a.dirs[name] == "" {
		return "⚪"
	}

	running, _, err := a.compose.status(ctx, a.dirs[name])
	if err != nil {
		return "⚪"
	}

	if running {
		return "🟢"
	}
	return "🔴"
}
func (a *App) setIssue(name, issue string) {
	a.issuesMu.Lock()
	defer a.issuesMu.Unlock()
	a.issues[name] = issue
}
func (a *App) clearIssue(name string) {
	a.issuesMu.Lock()
	defer a.issuesMu.Unlock()
	delete(a.issues, name)
}
func (a *App) issueSnapshot() map[string]string {
	a.issuesMu.Lock()
	defer a.issuesMu.Unlock()
	m := make(map[string]string, len(a.issues))
	for k, v := range a.issues {
		m[k] = v
	}
	return m
}
func (a *App) statusMessage() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hermes Fleet Bridge status (%s UTC)", time.Now().UTC().Format("15:04:05"))
	fmt.Fprintf(&b, "\n- Serving: %d member(s)", len(a.active))
	for _, n := range a.active {
		status := a.memberStatus(n)
		fmt.Fprintf(&b, "\n- %s %s -> %s", status, n, topicName(n))
	}
	issues := a.issueSnapshot()
	b.WriteString("\n- Issues: none")
	for _, n := range a.all {
		if msg, ok := issues[n]; ok {
			fmt.Fprintf(&b, "\n  - %s: %s", n, msg)
		}
	}
	return b.String()
}
func (a *App) poll(ctx context.Context) {
	for ctx.Err() == nil {
		offset := a.state.offset()
		updates, err := a.telegram.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("getUpdates: %v", err)
			sleep(ctx, 5*time.Second)
			continue
		}
		for _, u := range updates {
			if ctx.Err() != nil {
				return
			}
			agent := a.routeUpdate(u)
			stored, err := a.state.ingest(u, agent)
			if err != nil {
				log.Printf("FATAL: cannot persist Telegram update %d: %v", u.UpdateID, err)
				return
			}
			if stored {
				log.Printf("queued Telegram message %d for %s", u.Message.MessageID, agent)
			}
		}
	}
}
func (a *App) agentWorker(ctx context.Context, agent string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		p := a.state.first(agent)
		if p != nil {
			a.busy[agent].Store(true)
			err := a.processMessage(ctx, agent, p)
			if err != nil && ctx.Err() == nil {
				log.Printf("agent %s message %d attempt %d failed: %v", agent, p.MessageID, p.Attempts+1, err)
				p.Attempts++
				if p.Attempts >= 3 && p.Reply == "" {
					p.Reply = "⚠️ Hermes couldn't process this message: " + safeErr(err)
				}
				if e := a.state.updatePending(agent, p); e != nil {
					log.Printf("FATAL state update: %v", e)
					return
				}
				sleep(ctx, time.Duration(p.Attempts*4)*time.Second)
			}
			a.busy[agent].Store(false)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func safeErr(err error) string {
	s := err.Error()
	if len(s) > 250 {
		s = s[:250]
	}
	return s
}
func (a *App) processMessage(ctx context.Context, agent string, p *Pending) error {
	// Start typing immediately, including during container cold boot.
	topic, ok := a.state.topic(agent)
	if !ok {
		return fmt.Errorf("topic not yet created")
	}
	typingCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(4 * time.Second)
		defer t.Stop()
		a.telegram.typing(typingCtx, topic.ID)
		for {
			select {
			case <-typingCtx.Done():
				return
			case <-t.C:
				a.telegram.typing(typingCtx, topic.ID)
			}
		}
	}()
	if p.Reply == "" {
		h, err := a.ensureHermes(ctx, agent)
		if err != nil {
			return err
		}
		if p.RunID == "" {
			session, sessionKey := sessionIDs(a.cfg.GroupID, agent)
			runID, err := h.startRun(ctx, session, sessionKey, p.Text, fmt.Sprintf("fleet-%d-%d", a.cfg.GroupID, p.MessageID))
			if err != nil {
				return err
			}
			p.RunID = runID
			if err := a.state.updatePending(agent, p); err != nil {
				return err
			}
			log.Printf("agent %s started Hermes run %s", agent, runID)
		}
		runCtx, cancelRun := context.WithTimeout(ctx, a.cfg.RunTimeout)
		status, err := h.wait(runCtx, p.RunID)
		cancelRun()
		if errors.Is(err, context.DeadlineExceeded) {
			// A run that exceeds the cap must not keep the fleet alive indefinitely.
			cancelCtx, cancelStop := context.WithTimeout(context.Background(), 15*time.Second)
			_ = h.stopRun(cancelCtx, p.RunID)
			cancelStop()
			p.Reply = "⚠️ Hermes run exceeded the configured RUN_TIMEOUT and was asked to stop."
		} else if err != nil {
			return err
		} else {
			switch status.Status {
			case "completed":
				p.Reply = status.Output
				if strings.TrimSpace(p.Reply) == "" {
					p.Reply = "(Hermes completed with no text output.)"
				}
			default:
				p.Reply = "⚠️ Hermes run ended: " + status.Status
				if status.Error != nil {
					p.Reply += " (see Hermes logs for details)"
				}
			}
		}
		if err := a.state.updatePending(agent, p); err != nil {
			return err
		}
	}
	// The reply is persisted before sending: failed sends never rerun the agent.
	chunks := splitTelegram(p.Reply)
	for p.Sent < len(chunks) {
		topic, ok := a.state.topic(agent)
		if !ok {
			return fmt.Errorf("missing topic mapping")
		}
		sendCtx, cancelSend := context.WithTimeout(ctx, 20*time.Second)
		err := a.telegram.sendMessage(sendCtx, topic.ID, chunks[p.Sent])
		cancelSend()
		if err != nil && isMissingTopic(err) {
			if e := a.ensureTopic(ctx, agent, true); e != nil {
				return e
			}
			topic, _ = a.state.topic(agent)
			sendCtx, retryCancel := context.WithTimeout(ctx, 20*time.Second)
			err = a.telegram.sendMessage(sendCtx, topic.ID, chunks[p.Sent])
			retryCancel()
		}
		if err != nil {
			return err
		}
		p.Sent++
		if err := a.state.updatePending(agent, p); err != nil {
			return err
		}
	}
	return a.state.finish(agent, p.UpdateID)
}
func sessionIDs(groupID int64, agent string) (string, string) {
	key := fmt.Sprintf("telegram:group:%d:container:%s", groupID, agent)
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(key))
	return fmt.Sprintf("fleet_%016x", hasher.Sum64()), key
}
func (a *App) ensureHermes(ctx context.Context, agent string) (*Hermes, error) {
	lock := a.lifecycle[agent]
	lock.Lock()
	defer lock.Unlock()

	// Own the API server contract in the member's persistent .env. Hermes loads
	// that file with override=true, so it (not the Docker env) is authoritative.
	key, changed, err := a.compose.ensureAPIEnv(ctx, a.dir(agent), strconv.Itoa(a.cfg.DefaultPort), a.state.apiKey(agent))
	if err != nil {
		return nil, err
	}
	if err := a.state.setAPIKey(agent, key); err != nil {
		return nil, err
	}

	running, _, err := a.compose.status(ctx, a.dir(agent))
	if err != nil {
		return nil, err
	}
	if !running || changed {
		if changed && running {
			log.Printf("member %s: API server env changed; recreating", agent)
		} else {
			log.Printf("waking member %s", agent)
		}
		// Notify General that this member is starting
		a.notifyGeneral(fmt.Sprintf("🔄 Member %s: starting...", agent))
		if err := a.compose.up(ctx, a.dir(agent), changed && running); err != nil {
			a.notifyGeneral(fmt.Sprintf("❌ Member %s: failed to start: %v", agent, err))
			return nil, err
		}
	}

	// Wait for the member's API server to answer /health.
	deadlineCtx, cancel := context.WithTimeout(ctx, a.cfg.StartTimeout)
	defer cancel()
	var lastErr error
	for deadlineCtx.Err() == nil {
		_, primary, serr := a.compose.status(deadlineCtx, a.dir(agent))
		if serr != nil {
			lastErr = serr
		} else if primary != "" {
			info, ierr := a.docker.inspect(deadlineCtx, primary)
			if ierr != nil {
				lastErr = ierr
			} else if info.State.Running {
				base, eerr := a.cfg.hermesEndpoint(info)
				if eerr != nil {
					lastErr = eerr
				} else {
					h := newHermes(base, key)
					if rerr := h.ready(deadlineCtx); rerr == nil {
						a.clearIssue(agent)
						a.notifyGeneral(fmt.Sprintf("✅ Member %s: started", agent))
						return h, nil
					} else {
						lastErr = rerr
					}
				}
			}
		}
		sleep(deadlineCtx, 2*time.Second)
	}
	return nil, fmt.Errorf("member %s not ready after %s: %v", agent, a.cfg.StartTimeout, lastErr)
}
func (a *App) idleMonitor(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.stopIdle(ctx)
		}
	}
}
func (a *App) stopIdle(ctx context.Context) {
	for _, agent := range a.active {
		if a.busy[agent].Load() || !a.state.idle(agent, time.Now(), a.cfg.IdleTimeout) {
			continue
		}
		lock := a.lifecycle[agent]
		lock.Lock()
		// Recheck after taking the lock: a new message may have been accepted in between.
		if !a.busy[agent].Load() && a.state.idle(agent, time.Now(), a.cfg.IdleTimeout) {
			running, _, err := a.compose.status(ctx, a.dir(agent))
			if err != nil {
				log.Printf("idle status %s: %v", agent, err)
			} else if running {
				log.Printf("stopping idle member %s", agent)
				a.notifyGeneral(fmt.Sprintf("🛑 Member %s: stopping (idle)", agent))
				if err := a.compose.down(ctx, a.dir(agent)); err != nil {
					log.Printf("down %s: %v", agent, err)
				}
			}
		}
		lock.Unlock()
	}
}
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
