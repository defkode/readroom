package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkerConfigTemplateLoads(t *testing.T) {
	plan, err := loadWorkerConfig(writeConfig(t, workerConfigTemplate))
	if err != nil {
		t.Fatal(err)
	}
	if plan.summaries == nil || plan.speech == nil {
		t.Fatal("the template should configure both queues")
	}
	if plan.summariesWait != 5*time.Minute || plan.speechWait != time.Minute {
		t.Errorf("waits = %s, %s", plan.summariesWait, plan.speechWait)
	}
}

func TestWorkerConfig(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte("s3cret\n"), 0o600)

	plan, err := loadWorkerConfig(writeConfig(t, `
server = "http://reader"
token_file = "`+tokenFile+`"
[summaries]
command = "/usr/bin/my-llm --fast"
every = "30s"
`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.token != "s3cret" {
		t.Errorf("token = %q", plan.token)
	}
	if plan.speech != nil {
		t.Error("speech should be off without its section")
	}
	if _, label, _ := plan.summaries.asker(nil, 0); label != "my-llm" {
		t.Errorf("label = %q", label)
	}
	if plan.summariesWait != 30*time.Second {
		t.Errorf("wait = %s", plan.summariesWait)
	}

	for name, body := range map[string]string{
		"unknown setting": "server = \"x\"\n[speech]\ncommand = \"c\"\nvoise = \"v\"\n",
		"no server":       "[speech]\ncommand = \"c\"\n",
		"nothing to do":   "server = \"x\"\n",
		"both backends":   "server = \"x\"\n[summaries]\napi = \"a\"\ncommand = \"c\"\n",
		"no backend":      "server = \"x\"\n[summaries]\nmodel = \"m\"\n",
		"bad backend":     "server = \"x\"\n[summaries]\nbackend = \"gpt\"\n",
		"every too short": "server = \"x\"\n[speech]\ncommand = \"c\"\nevery = \"1s\"\n",
		"no command":      "server = \"x\"\n[speech]\nvoice = \"v\"\n",
		"two tokens":      "server = \"x\"\ntoken = \"a\"\ntoken_file = \"b\"\n[speech]\ncommand = \"c\"\n",
	} {
		if _, err := loadWorkerConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fakeReader is the part of the server's API the worker uses.
type fakeReader struct {
	mu        sync.Mutex
	pending   []int64
	speak     []int64
	summaries map[int64]map[string]string
	audio     map[int64]string
	auth      []string
}

func (f *fakeReader) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	entries := func(ids []int64) []map[string]any {
		out := []map[string]any{}
		if len(ids) > 0 {
			out = append(out, map[string]any{"id": ids[0], "title": "Title", "text": "Body text"})
		}
		return out
	}
	var id int64
	switch {
	case r.URL.Path == "/api/v1/summaries/pending":
		json.NewEncoder(w).Encode(map[string]any{"entries": entries(f.pending), "remaining": len(f.pending)})
	case r.URL.Path == "/api/v1/audio/pending":
		json.NewEncoder(w).Encode(map[string]any{"entries": entries(f.speak)})
	case r.Method == "PUT" && scan(r.URL.Path, "/api/v1/entries/%d/summary", &id):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.summaries[id] = body
		f.pending = f.pending[1:]
	case r.Method == "PUT" && scan(r.URL.Path, "/api/v1/entries/%d/audio", &id):
		data, _ := io.ReadAll(r.Body)
		f.audio[id] = r.Header.Get("Content-Type") + "|" + r.URL.Query().Get("voice") + "|" + string(data)
		f.speak = f.speak[1:]
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

func scan(path, format string, id *int64) bool {
	parts, want := strings.Split(path, "/"), strings.Split(format, "/")
	if len(parts) != len(want) {
		return false
	}
	for i := range want {
		if want[i] == "%d" {
			var n int64
			for _, ch := range parts[i] {
				if ch < '0' || ch > '9' {
					return false
				}
				n = n*10 + int64(ch-'0')
			}
			*id = n
		} else if want[i] != parts[i] {
			return false
		}
	}
	return true
}

func TestWorkerEmptiesBothQueues(t *testing.T) {
	fake := &fakeReader{pending: []int64{1, 2, 3}, speak: []int64{7},
		summaries: map[int64]map[string]string{}, audio: map[int64]string{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	// The summarising command proves it was handed both the instructions and the
	// article; the speaking one echoes what it was told to read.
	plan, err := loadWorkerConfig(writeConfig(t, `
server = "`+srv.URL+`/"
token = "tok"
[summaries]
command = "grep -c -e 'summarise articles' -e 'Body text'"
label = "counter"
[speech]
command = "cat"
voice = "Echo"
type = "audio/wav"
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := workerRun(plan, true); err != nil {
		t.Fatal(err)
	}
	if len(fake.summaries) != 3 {
		t.Fatalf("summaries written: %d", len(fake.summaries))
	}
	if got := fake.summaries[2]; got["summary"] != "2" || got["model"] != "counter" {
		t.Errorf("summary = %v", got)
	}
	if got := fake.audio[7]; got != "audio/wav|Echo|Title.\nBody text" {
		t.Errorf("audio = %q", got)
	}
	for _, a := range fake.auth {
		if a != "Bearer tok" {
			t.Fatalf("authorization = %q", a)
		}
	}
}

func TestWorkLoopKeepsGoingAfterAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := workLoop(ctx, "test", time.Millisecond, false, func(context.Context) (bool, error) {
		calls++
		if calls == 3 {
			cancel()
		}
		return false, io.ErrUnexpectedEOF
	})
	if err != nil || calls != 3 {
		t.Errorf("err = %v after %d calls", err, calls)
	}
}

func TestUnitText(t *testing.T) {
	unit := unitText("/home/u/.local/bin/readroom", "/home/u/.config/readroom/worker.toml", "/a:/b%c")
	for _, want := range []string{
		`ExecStart="/home/u/.local/bin/readroom" worker -config "/home/u/.config/readroom/worker.toml"`,
		`Environment="PATH=/a:/b%%c"`,
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %s:\n%s", want, unit)
		}
	}
}

func TestParseBatch(t *testing.T) {
	got, ok := parseBatch("Here you go:\n<1>Jeden</1>\n<2>Dwa <a href=\"/x\">trzy</a></2>", 2)
	if !ok || got[0] != "Jeden" || got[1] != `Dwa <a href="/x">trzy</a>` {
		t.Errorf("parsed %q, %v", got, ok)
	}
	for name, answer := range map[string]string{
		"one missing":  "<1>Jeden</1>",
		"one twice":    "<1>Jeden</1><1>Jeden</1><2>Dwa</2>",
		"one empty":    "<1>Jeden</1><2> </2>",
		"out of range": "<1>Jeden</1><3>Trzy</3>",
	} {
		if _, ok := parseBatch(answer, 2); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Pieces go to the model a few at a time; an answer that cannot be taken apart
// is asked for again piece by piece.
func TestTranslatePieces(t *testing.T) {
	pieces := []string{"one", "two", strings.Repeat("long ", 600), "four", "five"}
	calls := 0
	ask := func(_ context.Context, instructions, text string) (string, error) {
		calls++
		if !strings.Contains(instructions, "into Polish") {
			t.Errorf("instructions do not name the language: %s", instructions)
		}
		if strings.HasPrefix(text, "<1>") {
			if strings.Contains(text, "four") {
				return "I would rather not use tags.", nil
			}
			return numbered.ReplaceAllString(text, "<$1>PL $2</$3>"), nil
		}
		return "PL " + text, nil
	}
	got, err := translatePieces(context.Background(), ask, "Polish", pieces)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range pieces {
		if got[i] != "PL "+strings.TrimSpace(p) && got[i] != "PL "+p {
			t.Errorf("piece %d = %.40q", i, got[i])
		}
	}
	// "one" and "two" as a batch; the long piece by itself, as it fits with nothing;
	// "four" and "five" as a batch whose answer is refused, so each is asked for again.
	if calls != 5 {
		t.Errorf("model asked %d times", calls)
	}
}
