package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

// The worker is the client side of Readroom in one piece: on a computer that has
// the models it keeps both queues empty — summaries and recordings — from one
// configuration file. It does nothing the "summarize" and "speak" commands do
// not; it only saves setting them up one by one.

const workerUnit = "readroom-worker.service"

// The units the worker replaces. Two clients on one queue would do the work twice.
var oldUnits = []string{"readroom-summarize.timer", "readroom-speak.timer"}

const workerConfigTemplate = `# Readroom worker: writes the summaries and records the articles asked to be read
# aloud, on this computer, for the Readroom server below.
#
#   readroom worker check     try every part once, without changing anything
#   readroom worker install   run it in the background from now on (systemd)

server = "http://127.0.0.1:8090"

# The server's access token: here, or alone in a file only you can read.
# token = "..."
# token_file = "~/.config/readroom/token"

# Remove a section to leave that work to someone else.

[summaries]
# Any server that speaks the OpenAI API: Ollama, llama.cpp, LM Studio, a hosted one.
api = "http://127.0.0.1:11434/v1"
model = "llama3.2"
# api_key = "..."

# Or the claude CLI, run with every tool switched off:
# backend = "claude"
# model = "haiku"

# Or any command: it gets the instructions and the article on standard input and
# prints the summary. The article is untrusted text, so give it a command that
# can only write text, not one that can act on what it reads.
# command = "llm -m gpt-5-mini"
# label = "gpt-5-mini"            # the name shown with the summary

# every = "5m"                    # how often the queue is checked

[speech]
# Any command that reads text on standard input and writes audio to standard
# output. Whatever ffmpeg can read is encoded to MP3.
command = "tiny-voca-speak"
voice = "tiny-voca M2"            # the name shown with the recording
# type = "audio/ogg"              # upload the command's output as it is, without ffmpeg
# bitrate = "48k"
# every = "1m"
`

type workerConfig struct {
	Server    string `toml:"server"`
	Token     string `toml:"token"`
	TokenFile string `toml:"token_file"`
	Summaries *struct {
		Backend  string `toml:"backend"`
		API      string `toml:"api"`
		APIKey   string `toml:"api_key"`
		Model    string `toml:"model"`
		Command  string `toml:"command"`
		Label    string `toml:"label"`
		MaxChars int    `toml:"max_chars"`
		Every    string `toml:"every"`
	} `toml:"summaries"`
	Speech *struct {
		Command string `toml:"command"`
		Voice   string `toml:"voice"`
		Type    string `toml:"type"`
		Bitrate string `toml:"bitrate"`
		Every   string `toml:"every"`
	} `toml:"speech"`
}

// workerPlan is the configuration after it has been checked: what to run and how often.
type workerPlan struct {
	server, token string
	summaries     *summaryOpts
	speech        *speechOpts
	summariesWait time.Duration
	speechWait    time.Duration
}

func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

func parseEvery(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 10*time.Second {
		return 0, fmt.Errorf(`every = %q: must be a duration of at least 10s, such as "5m"`, s)
	}
	return d, nil
}

func loadWorkerConfig(path string) (*workerPlan, error) {
	var cfg workerConfig
	meta, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf(`%s does not exist: "readroom worker init" writes one to fill in`, path)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if extra := meta.Undecoded(); len(extra) > 0 {
		return nil, fmt.Errorf("%s: unknown setting %q", path, extra[0].String())
	}
	if cfg.Server == "" {
		return nil, fmt.Errorf("%s: server is not set", path)
	}
	p := &workerPlan{server: cfg.Server, token: cfg.Token}
	switch {
	case cfg.Token != "" && cfg.TokenFile != "":
		return nil, fmt.Errorf("%s: set token or token_file, not both", path)
	case cfg.TokenFile != "":
		data, err := os.ReadFile(expandHome(cfg.TokenFile))
		if err != nil {
			return nil, fmt.Errorf("token_file: %w", err)
		}
		if p.token = strings.TrimSpace(string(data)); p.token == "" {
			return nil, fmt.Errorf("token_file: %s is empty", cfg.TokenFile)
		}
	case cfg.Token == "":
		p.token = os.Getenv("READROOM_TOKEN")
	}
	if s := cfg.Summaries; s != nil {
		p.summaries = &summaryOpts{Backend: s.Backend, API: s.API, APIKey: s.APIKey, Model: s.Model,
			Command: s.Command, Label: s.Label, MaxChars: s.MaxChars}
		if s.API != "" && s.Command != "" {
			return nil, errors.New("[summaries]: set api or command, not both")
		}
		if _, _, err := p.summaries.asker(nil); err != nil {
			return nil, fmt.Errorf("[summaries]: %w", err)
		}
		if p.summariesWait, err = parseEvery(s.Every, 5*time.Minute); err != nil {
			return nil, fmt.Errorf("[summaries]: %w", err)
		}
	}
	if s := cfg.Speech; s != nil {
		if strings.TrimSpace(s.Command) == "" {
			return nil, errors.New("[speech]: command is not set")
		}
		p.speech = &speechOpts{Command: s.Command, Voice: s.Voice, Type: s.Type, Bitrate: s.Bitrate}
		if p.speechWait, err = parseEvery(s.Every, time.Minute); err != nil {
			return nil, fmt.Errorf("[speech]: %w", err)
		}
	}
	if p.summaries == nil && p.speech == nil {
		return nil, fmt.Errorf("%s: neither [summaries] nor [speech] is there, so there is nothing to do", path)
	}
	return p, nil
}

func defaultWorkerConfig() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "worker.toml"
	}
	return filepath.Join(dir, "readroom", "worker.toml")
}

func worker(args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	config := fs.String("config", defaultWorkerConfig(), "configuration file")
	once := fs.Bool("once", false, "with run: empty both queues once and exit, for cron and the like")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: readroom worker [-config file] [command]

Writes the summaries and records the articles asked to be read aloud, for a
Readroom server somewhere else, with the models on this computer.

  init        write a configuration file to fill in
  check       try every part once: the server, the token, the model, the voice
  run         keep both queues empty until stopped (the default; -once to stop when they are)
  install     run it in the background from now on, as a systemd user service
  uninstall   stop that and remove the service`)
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	cmd := "run"
	if fs.NArg() > 0 {
		cmd = fs.Arg(0)
	}
	path, err := filepath.Abs(expandHome(*config))
	if err != nil {
		return err
	}
	switch cmd {
	case "init":
		return workerInit(path)
	case "uninstall":
		return workerUninstall()
	}
	plan, err := loadWorkerConfig(path)
	if err != nil {
		return err
	}
	switch cmd {
	case "check":
		return workerCheck(plan)
	case "install":
		return workerInstall(path)
	case "run":
		return workerRun(plan, *once)
	}
	fs.Usage()
	os.Exit(2)
	return nil
}

func workerInit(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s is already there", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// It may come to hold the token, so it is the owner's alone from the start.
	if err := os.WriteFile(path, []byte(workerConfigTemplate), 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s\nfill it in, then try it with: readroom worker check\n", path)
	return nil
}

func workerRun(plan *workerPlan, once bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := newClient(plan.server, plan.token)

	var wg sync.WaitGroup
	var failed error
	var mu sync.Mutex
	start := func(name string, wait time.Duration, pass func(context.Context) (bool, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := workLoop(ctx, name, wait, once, pass); err != nil {
				mu.Lock()
				failed = errors.Join(failed, fmt.Errorf("%s: %w", name, err))
				mu.Unlock()
			}
		}()
	}
	if o := plan.summaries; o != nil {
		logf := func(format string, a ...any) { printf("summaries: "+format, a...) }
		start("summaries", plan.summariesWait, func(ctx context.Context) (bool, error) {
			done, skipped, _, err := runSummaries(ctx, c, *o, logf)
			return done+skipped > 0, err
		})
	}
	if o := plan.speech; o != nil {
		logf := func(format string, a ...any) { printf("speech: "+format, a...) }
		start("speech", plan.speechWait, func(ctx context.Context) (bool, error) {
			done, failed, err := runSpeech(ctx, c, *o, logf)
			return done+failed > 0, err
		})
	}
	if !once {
		fmt.Printf("readroom worker %s: serving %s\n", version, plan.server)
	}
	wg.Wait()
	return failed
}

// workLoop empties one queue, waits, and does it again. A pass that fails — the
// server out of reach, the model not started yet — is tried again after the same
// wait; the reason is logged once, not every time, and so is the recovery.
func workLoop(ctx context.Context, name string, wait time.Duration, once bool, pass func(context.Context) (bool, error)) error {
	lastErr := ""
	for {
		_, err := pass(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if once {
			return err
		}
		switch {
		case err != nil && err.Error() != lastErr:
			lastErr = err.Error()
			fmt.Printf("%s: %v (trying again every %s)\n", name, err, wait)
		case err == nil && lastErr != "":
			lastErr = ""
			fmt.Printf("%s: working again\n", name)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

const checkSample = `Title: A lighthouse keeps its lamp

The lighthouse on the northern headland will keep its original lamp after the harbour board voted on Tuesday against replacing it with a modern beacon. Volunteers have maintained the mechanism since 1998 and raised the money for new bearings last year. The board said the old lamp is visible from far enough for the fishing boats that still use the harbour, and that the visitors it draws pay for its upkeep.`

// workerCheck tries each part once and changes nothing on the server.
func workerCheck(plan *workerPlan) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := newClient(plan.server, plan.token)
	bad := 0
	report := func(name string, err error, ok string) {
		if err != nil {
			bad++
			fmt.Printf("FAIL  %-10s %v\n", name, err)
			return
		}
		fmt.Printf("ok    %-10s %s\n", name, ok)
	}

	var queue struct {
		Entries   []struct{} `json:"entries"`
		Remaining int        `json:"remaining"`
	}
	err := c.do(ctx, "GET", "/api/v1/summaries/pending?limit=1&max_chars=1", nil, &queue)
	report("server", err, fmt.Sprintf("%s answers and accepts the token", plan.server))

	if o := plan.summaries; o != nil {
		ask, label, _ := o.asker(c.http)
		start := time.Now()
		summary, err := ask(ctx, checkSample)
		if err == nil && summary == "" {
			err = errors.New("the model returned nothing")
		}
		report("summaries", err, fmt.Sprintf("%s in %.1fs: %s", label, time.Since(start).Seconds(), summary))
	} else {
		fmt.Printf("--    %-10s not configured\n", "summaries")
	}

	if o := plan.speech; o != nil {
		work, err := os.MkdirTemp("", "readroom-check")
		if err == nil {
			defer os.RemoveAll(work)
			if o.Type == "" {
				_, err = exec.LookPath("ffmpeg")
			}
		}
		var audio []byte
		kind := ""
		start := time.Now()
		if err == nil {
			bitrate := o.Bitrate
			if bitrate == "" {
				bitrate = "48k"
			}
			audio, kind, err = synthesize(ctx, work, o.Command, o.Type, bitrate, "The lighthouse keeps its lamp.")
		}
		report("speech", err, fmt.Sprintf("%d kB of %s in %.1fs", len(audio)/1000, kind, time.Since(start).Seconds()))
	} else {
		fmt.Printf("--    %-10s not configured\n", "speech")
	}
	if bad > 0 {
		return fmt.Errorf("%d of the checks failed", bad)
	}
	return nil
}

// unitText is the systemd user service that keeps the worker running. A user
// service does not get the PATH of a login shell, where the text-to-speech
// command and tools like ffmpeg tend to live, so the PATH of whoever installs it
// is written in.
func unitText(exe, config, path string) string {
	esc := func(s string) string { return strings.ReplaceAll(s, "%", "%%") }
	return fmt.Sprintf(`# Written by "readroom worker install"; that command writes it again.
[Unit]
Description=Readroom worker: summaries and recordings for the reader
After=network-online.target

[Service]
ExecStart="%s" worker -config "%s"
Environment="PATH=%s"
Restart=on-failure
RestartSec=30
Nice=10

[Install]
WantedBy=default.target
`, esc(exe), esc(config), esc(path))
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, lastLine(string(out)))
	}
	return nil
}

func unitDir() (string, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "", errors.New(`there is no systemd here: start "readroom worker" by whatever runs programs in the background on this system`)
	}
	dir, err := os.UserConfigDir()
	return filepath.Join(dir, "systemd", "user"), err
}

func workerInstall(config string) error {
	dir, err := unitDir()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unit := filepath.Join(dir, workerUnit)
	if err := os.WriteFile(unit, []byte(unitText(exe, config, os.Getenv("PATH"))), 0o644); err != nil {
		return err
	}
	// The timers from before the worker did the same work: one client per queue.
	for _, old := range oldUnits {
		if exec.Command("systemctl", "--user", "is-enabled", "--quiet", old).Run() == nil {
			if err := systemctl("disable", "--now", old); err != nil {
				return err
			}
			fmt.Printf("stopped %s, which the worker replaces\n", old)
		}
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", workerUnit); err != nil {
		return err
	}
	if err := systemctl("restart", workerUnit); err != nil {
		return err
	}
	fmt.Printf(`wrote %s
the worker is running, and starts with your session from now on
  systemctl --user status %s
  journalctl --user -u %s -f
after changing %s: systemctl --user restart %s
`, unit, workerUnit, workerUnit, config, workerUnit)
	return nil
}

func workerUninstall() error {
	dir, err := unitDir()
	if err != nil {
		return err
	}
	unit := filepath.Join(dir, workerUnit)
	if _, err := os.Stat(unit); err != nil {
		return fmt.Errorf("%s is not installed", workerUnit)
	}
	if err := systemctl("disable", "--now", workerUnit); err != nil {
		return err
	}
	if err := os.Remove(unit); err != nil {
		return err
	}
	fmt.Printf("stopped the worker and removed %s\n", unit)
	return systemctl("daemon-reload")
}
