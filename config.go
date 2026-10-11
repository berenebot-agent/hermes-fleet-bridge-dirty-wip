package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Token                                 string
	GroupID                               int64
	Allowed                               map[int64]bool
	Members                               []Member
	IdleTimeout, StartTimeout, RunTimeout time.Duration
	DataDir, DockerSocket                 string
	DefaultPort                           int
	PreferredNetwork                      string
	// IconOverrides maps a member name to the exact Telegram emoji requested
	// as its topic icon. Empty means hash-based assignment.
	IconOverrides map[string]string
}

// Member is one fleet agent: a name (its topic/state identity) and the compose
// project directory that defines it.
type Member struct {
	Name string
	Dir  string
}

func loadConfig() (Config, error) {
	c := Config{Allowed: map[int64]bool{}, DockerSocket: getEnv("DOCKER_SOCKET", "/var/run/docker.sock"), DataDir: getEnv("DATA_DIR", "/data"), PreferredNetwork: os.Getenv("HERMES_NETWORK")}
	c.Token = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if c.Token == "" {
		return c, fmt.Errorf("TELEGRAM_BOT_TOKEN required")
	}
	var err error
	c.GroupID, err = strconv.ParseInt(strings.TrimSpace(os.Getenv("TELEGRAM_GROUP_ID")), 10, 64)
	if err != nil || c.GroupID == 0 {
		return c, fmt.Errorf("TELEGRAM_GROUP_ID must be a numeric supergroup id")
	}
	for _, s := range strings.Split(os.Getenv("TELEGRAM_ALLOWED_USER_IDS"), ",") {
		if strings.TrimSpace(s) == "" {
			continue
		}
		n, e := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if e != nil || n <= 0 {
			return c, fmt.Errorf("invalid allowed user ID %q", s)
		}
		c.Allowed[n] = true
	}
	if len(c.Allowed) == 0 {
		return c, fmt.Errorf("TELEGRAM_ALLOWED_USER_IDS required (fail closed)")
	}
	seen := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("HERMES_DIRS"), ",") {
		d := strings.TrimSpace(s)
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			return c, fmt.Errorf("HERMES_DIRS entry must be an absolute path: %q", d)
		}
		d = filepath.Clean(d)
		name := filepath.Base(d)
		if name == "." || name == "/" || name == "" {
			return c, fmt.Errorf("cannot derive agent name from directory %q", d)
		}
		if seen[name] {
			return c, fmt.Errorf("duplicate agent name %q (from %s)", name, d)
		}
		seen[name] = true
		c.Members = append(c.Members, Member{Name: name, Dir: d})
	}
	if len(c.Members) == 0 {
		return c, fmt.Errorf("HERMES_DIRS required (comma-separated absolute compose project directories)")
	}
	c.IdleTimeout, err = time.ParseDuration(getEnv("IDLE_TIMEOUT", "15m"))
	if err != nil || c.IdleTimeout < 10*time.Second {
		return c, fmt.Errorf("IDLE_TIMEOUT must be >=10s")
	}
	c.StartTimeout, err = time.ParseDuration(getEnv("START_TIMEOUT", "120s"))
	if err != nil || c.StartTimeout < 1*time.Second {
		return c, fmt.Errorf("invalid START_TIMEOUT")
	}
	c.RunTimeout, err = time.ParseDuration(getEnv("RUN_TIMEOUT", "60m"))
	if err != nil || c.RunTimeout < 1*time.Minute {
		return c, fmt.Errorf("RUN_TIMEOUT must be >=1m")
	}
	c.DefaultPort, err = strconv.Atoi(getEnv("HERMES_API_PORT", "8642"))
	if err != nil || c.DefaultPort < 1 || c.DefaultPort > 65535 {
		return c, fmt.Errorf("invalid HERMES_API_PORT")
	}
	if c.IconOverrides, err = parseIconOverrides(os.Getenv("HERMES_ICONS")); err != nil {
		return c, err
	}
	for name := range c.IconOverrides {
		if !seen[name] {
			return c, fmt.Errorf("HERMES_ICONS references unknown member %q", name)
		}
	}
	return c, nil
}

// parseIconOverrides parses HERMES_ICONS entries of the form "member=emoji",
// comma-separated. The emoji may be any Telegram emoji; whether Telegram
// accepts it as a topic icon is checked at runtime against the sticker pool.
func parseIconOverrides(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, emoji, ok := strings.Cut(entry, "=")
		name, emoji = strings.TrimSpace(name), strings.TrimSpace(emoji)
		if !ok || name == "" || emoji == "" || strings.ContainsAny(name, " =") {
			return nil, fmt.Errorf("invalid HERMES_ICONS entry %q (want member=emoji)", entry)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("duplicate HERMES_ICONS member %q", name)
		}
		out[name] = emoji
	}
	return out, nil
}
func getEnv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func topicName(container string) string {
	n := strings.TrimPrefix(container, "hermes-")
	n = strings.NewReplacer("-", " ", "_", " ").Replace(n)
	words := strings.Fields(n)
	for i, v := range words {
		if len(v) > 0 {
			words[i] = strings.ToUpper(v[:1]) + v[1:]
		}
	}
	n = strings.Join(words, " ")
	if n == "" {
		return container
	}
	r := []rune(n)
	if len(r) > 64 {
		n = string(r[:64])
	}
	return n
}

// topicIconColors are the only colours Telegram accepts for topic icons.
// The colour is fixed at creation; editForumTopic cannot change it.
var topicIconColors = []int{7322096, 16766590, 13338331, 9367192, 16749490, 16478047}

// pickTopicIcon derives a member's topic icon from its container name: the
// emoji (when the pool is non-empty) and the colour are hash-assigned, so the
// same name always yields the same icon without any state. A manual override
// wins.
func pickTopicIcon(name, override string, pool []string) forumTopicIcon {
	h := fnv.New64a()
	_, _ = h.Write([]byte("telegram:topic:icon:" + name))
	sum := h.Sum64()
	icon := forumTopicIcon{Color: topicIconColors[int(sum%uint64(len(topicIconColors)))]}
	if len(pool) > 0 {
		icon.Emoji = pool[int((sum>>32)%uint64(len(pool)))]
	}
	if override != "" {
		icon.Emoji = override
	}
	return icon
}
