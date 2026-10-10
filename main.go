package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"os/signal"
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
	telegram  *Telegram
	state     *Store
	busy      map[string]*atomic.Bool
	lifecycle map[string]*sync.Mutex
	all       []string
	issuesMu  sync.Mutex
	issues    map[string]string
}

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
	a := &App{cfg: cfg, docker: newDocker(cfg.DockerSocket), telegram: newTelegram(cfg.Token, cfg.GroupID), state: st, busy: map[string]*atomic.Bool{}, lifecycle: map[string]*sync.Mutex{}, all: append([]string(nil), cfg.Containers...), issues: map[string]string{}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.telegram.deleteWebhook(ctx); err != nil {
		log.Printf("Telegram webhook reset: %v", err)
	}
	started := time.Now().UTC()
	// Only service containers that exist; report the rest in General.
	var active []string
	for _, name := range cfg.Containers {
		a.busy[name] = &atomic.Bool{}
		a.lifecycle[name] = &sync.Mutex{}
		if err := st.touch(name); err != nil {
			log.Fatal("state write: ", err)
		}
		if _, err := a.docker.inspect(ctx, name); err != nil {
			if errors.Is(err, errContainerNotFound) {
				a.setIssue(name, "container not found")
				log.Printf("container %s not found; skipping topic, reporting in General", name)
			} else {
				a.setIssue(name, safeErr(err))
				log.Printf("container %s inspect failed: %v", name, err)
			}
			continue
		}
		active = append(active, name)
	}
	a.cfg.Containers = active
	log.Printf("bridge configured: %d/%d Hermes containers, group %d, idle %v; service messages in General", len(active), len(cfg.Containers), cfg.GroupID, cfg.IdleTimeout)
	go a.notifyGeneral(a.serviceStartMessage(active, started))
	for _, agent := range cfg.Containers {
		go a.agentWorker(ctx, agent)
	}
	go a.idleMonitor(ctx)
	go a.topicMonitor(ctx)
	a.poll(ctx)
}

func (a *App) topicMonitor(ctx context.Context) {
	a.syncTopics(ctx)
	a.checkContainers(ctx)
	tick := time.NewTicker(1 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.syncTopics(ctx)
			a.checkContainers(ctx)
		}
	}
}

// checkContainers re-inspects every configured container and reports issue
// transitions (appeared, changed, recovered) in General.
func (a *App) checkContainers(ctx context.Context) {
	before := a.issueSnapshot()
	for _, name := range a.all {
		if _, err := a.docker.inspect(ctx, name); err != nil {
			if errors.Is(err, errContainerNotFound) {
				a.setIssue(name, "container not found")
			} else {
				a.setIssue(name, safeErr(err))
			}
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
			a.notifyGeneral(fmt.Sprintf("⚠️ Container %s: %s", name, newMsg))
		case had && !has:
			a.notifyGeneral(fmt.Sprintf("✅ Container %s: recovered", name))
		}
	}
}
func (a *App) syncTopics(ctx context.Context) {
	for _, agent := range a.cfg.Containers {
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
	for _, agent := range a.cfg.Containers {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.telegram.sendMessage(ctx, generalThread, text); err != nil {
		log.Printf("General message failed: %v", err)
	}
}
func (a *App) serviceStartMessage(active []string, started time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hermes Fleet Bridge online (%s UTC)\n", started.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "Serving %d/%d containers, idle timeout %v", len(active), len(a.all), a.cfg.IdleTimeout)
	for _, n := range active {
		fmt.Fprintf(&b, "\n- %s -> %s", n, topicName(n))
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
	fmt.Fprintf(&b, "\n- Serving: %d container(s)", len(a.cfg.Containers))
	for _, n := range a.cfg.Containers {
		fmt.Fprintf(&b, "\n- %s -> %s", n, topicName(n))
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
	info, err := a.docker.inspect(ctx, agent)
	if err != nil {
		return nil, err
	}
	if !info.State.Running {
		log.Printf("starting sleeping container %s", agent)
		if err := a.docker.start(ctx, agent); err != nil {
			return nil, err
		}
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, a.cfg.StartTimeout)
	defer cancel()
	var lastErr error
	for deadlineCtx.Err() == nil {
		info, lastErr = a.docker.inspect(deadlineCtx, agent)
		if lastErr == nil && info.State.Running {
			var base, key string
			base, key, lastErr = a.cfg.hermesEndpoint(info)
			if lastErr == nil {
				h := newHermes(base, key)
				if lastErr = h.ready(deadlineCtx); lastErr == nil {
					a.clearIssue(agent)
					return h, nil
				}
			}
		}
		sleep(deadlineCtx, 2*time.Second)
	}
	return nil, fmt.Errorf("Hermes %s not ready after %s: %v", agent, a.cfg.StartTimeout, lastErr)
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
	for _, agent := range a.cfg.Containers {
		if a.busy[agent].Load() || !a.state.idle(agent, time.Now(), a.cfg.IdleTimeout) {
			continue
		}
		lock := a.lifecycle[agent]
		lock.Lock()
		// Recheck after taking the lock: a new message may have been accepted in between.
		if !a.busy[agent].Load() && a.state.idle(agent, time.Now(), a.cfg.IdleTimeout) {
			info, err := a.docker.inspect(ctx, agent)
			if err != nil {
				log.Printf("idle inspect %s: %v", agent, err)
			} else if info.State.Running {
				log.Printf("stopping idle container %s", agent)
				if err := a.docker.stop(ctx, agent); err != nil {
					log.Printf("stop %s: %v", agent, err)
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
