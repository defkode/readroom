package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// speak is a plain client of the REST API: it takes the server's queue of
// articles the owner asked to hear, has a command turn each into speech, and
// uploads the recording. It knows nothing about the voice: the command is any
// program that reads text on standard input and writes audio to standard output.
func speak(args []string) error {
	fs := flag.NewFlagSet("speak", flag.ExitOnError)
	apiURL := fs.String("api", env("READROOM_URL", "http://127.0.0.1:8090"), "readroom base URL")
	token := fs.String("token", os.Getenv("READROOM_TOKEN"), "readroom access token")
	command := fs.String("cmd", os.Getenv("READROOM_TTS"), "text-to-speech command, run by the shell: reads the text on standard input, writes audio to standard output")
	voice := fs.String("voice", "", "name shown with the recording, such as the model and its voice")
	ctype := fs.String("type", "", `content type of what the command writes, uploaded as it is (e.g. "audio/ogg"); by default the output is taken to be anything ffmpeg reads, such as WAV, and encoded to MP3`)
	bitrate := fs.String("bitrate", "48k", "MP3 bitrate (ignored with -type)")
	limit := fs.Int("limit", 10, "stop after this many articles in one run (0 = until the queue is empty)")
	fs.Parse(args)
	if strings.TrimSpace(*command) == "" {
		return errors.New(`no text-to-speech command: pass -cmd (or set READROOM_TTS), e.g. -cmd deploy/tts/tiny-voca-speak`)
	}

	c := &client{base: strings.TrimRight(*apiURL, "/"), token: *token, http: &http.Client{Timeout: 10 * time.Minute}}
	work, err := os.MkdirTemp("", "readroom-speak")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	// One article at a time, as with summaries: the server says what is wanted and
	// hands over text ready to be read; nothing is held here between runs.
	type item struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	next := func(skip []string) (*item, error) {
		path := "/api/v1/audio/pending?limit=1"
		if len(skip) > 0 {
			path += "&exclude=" + strings.Join(skip, ",")
		}
		var queue struct {
			Entries []item `json:"entries"`
		}
		if err := c.do("GET", path, nil, &queue); err != nil {
			return nil, err
		}
		if len(queue.Entries) == 0 {
			return nil, nil
		}
		return &queue.Entries[0], nil
	}

	var failed []string
	done := 0
	for *limit <= 0 || done+len(failed) < *limit {
		e, err := next(failed)
		if err != nil {
			return err
		}
		if e == nil || slices.Contains(failed, fmt.Sprint(e.ID)) {
			break
		}
		start := time.Now()
		// The title is read first, as its own paragraph.
		text := e.Text
		if title := strings.TrimSpace(e.Title); title != "" {
			if !strings.ContainsAny(title[len(title)-1:], ".!?") {
				title += "."
			}
			text = title + "\n" + text
		}
		audio, kind, err := synthesize(work, *command, *ctype, *bitrate, text)
		if err != nil {
			// Say so on the article, where the owner is waiting, and go on to the next.
			failed = append(failed, fmt.Sprint(e.ID))
			fmt.Printf("fail  #%d %s: %v\n", e.ID, e.Title, err)
			if err := c.do("POST", fmt.Sprintf("/api/v1/entries/%d/audio/error", e.ID), map[string]string{"error": err.Error()}, nil); err != nil {
				return err
			}
			continue
		}
		if err := c.upload(fmt.Sprintf("/api/v1/entries/%d/audio?voice=%s", e.ID, url.QueryEscape(*voice)), kind, audio); err != nil {
			return err
		}
		done++
		fmt.Printf("done  #%d %s (%.1f MB, %.1fs)\n", e.ID, e.Title, float64(len(audio))/(1<<20), time.Since(start).Seconds())
	}
	if done == 0 && len(failed) == 0 {
		fmt.Println("nothing to read aloud")
	} else {
		fmt.Printf("%d recorded, %d failed\n", done, len(failed))
	}
	return nil
}

// synthesize runs the text-to-speech command on text and returns the audio to
// upload with its content type.
func synthesize(work, command, ctype, bitrate, text string) ([]byte, string, error) {
	raw := filepath.Join(work, "speech")
	out, err := os.Create(raw)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	out.Close()
	if err != nil {
		return nil, "", fmt.Errorf("text-to-speech command: %w: %s", err, lastLine(stderr.String()))
	}
	if info, err := os.Stat(raw); err != nil || info.Size() == 0 {
		return nil, "", errors.New("text-to-speech command wrote no audio: " + lastLine(stderr.String()))
	}
	if ctype == "" {
		// MP3 plays in every browser and seeks without an index.
		mp3 := filepath.Join(work, "speech.mp3")
		enc := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", raw,
			"-vn", "-ac", "1", "-c:a", "libmp3lame", "-b:a", bitrate, mp3)
		if msg, err := enc.CombinedOutput(); err != nil {
			return nil, "", fmt.Errorf("ffmpeg: %w: %s", err, lastLine(string(msg)))
		}
		raw, ctype = mp3, "audio/mpeg"
	}
	data, err := os.ReadFile(raw)
	return data, ctype, err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// upload sends a file as the body of a PUT.
func (c *client) upload(path, ctype string, data []byte) error {
	req, err := http.NewRequest("PUT", c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return fmt.Errorf("PUT %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
