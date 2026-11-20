package main

import (
	"bytes"
	"context"
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

// speechVoice is one way of turning text into a recording.
type speechVoice struct {
	Command string // run by the shell: text on standard input, audio on standard output
	Voice   string // name shown with the recording
	Type    string // content type of what the command writes; empty = encode to MP3 with ffmpeg
	Bitrate string
}

// speechOpts says what makes the recordings and how much of the queue one run takes.
type speechOpts struct {
	speechVoice
	// For holds the voices for particular languages, by code ("pl"): a text in one
	// of them is read by its voice instead. Texts in any other language, or of
	// unknown language, are read by the voice above.
	For   map[string]speechVoice
	Limit int // stop after this many articles (0 = until the queue is empty)
}

// voiceFor picks the voice for a text in language.
func (o speechOpts) voiceFor(language string) speechVoice {
	v, ok := o.For[language]
	if !ok {
		v = o.speechVoice
	}
	if v.Bitrate == "" {
		v.Bitrate = "48k"
	}
	return v
}

// speak is a plain client of the REST API: it takes the server's queue of
// articles the owner asked to hear, has a command turn each into speech, and
// uploads the recording. It knows nothing about the voice: the command is any
// program that reads text on standard input and writes audio to standard output.
func speak(args []string) error {
	fs := flag.NewFlagSet("speak", flag.ExitOnError)
	apiURL := fs.String("api", env("READROOM_URL", "http://127.0.0.1:8090"), "readroom base URL")
	token := fs.String("token", os.Getenv("READROOM_TOKEN"), "readroom access token")
	var o speechOpts
	fs.StringVar(&o.Command, "cmd", os.Getenv("READROOM_TTS"), "text-to-speech command, run by the shell: reads the text on standard input, writes audio to standard output")
	fs.StringVar(&o.Voice, "voice", "", "name shown with the recording, such as the model and its voice")
	fs.StringVar(&o.Type, "type", "", `content type of what the command writes, uploaded as it is (e.g. "audio/ogg"); by default the output is taken to be anything ffmpeg reads, such as WAV, and encoded to MP3`)
	fs.StringVar(&o.Bitrate, "bitrate", "48k", "MP3 bitrate (ignored with -type)")
	fs.IntVar(&o.Limit, "limit", 10, "stop after this many articles in one run (0 = until the queue is empty)")
	fs.Parse(args)

	done, failed, err := runSpeech(context.Background(), newClient(*apiURL, *token), o, printf)
	if err != nil {
		return err
	}
	if done == 0 && failed == 0 {
		fmt.Println("nothing to read aloud")
	} else {
		fmt.Printf("%d recorded, %d failed\n", done, failed)
	}
	return nil
}

// runSpeech records the queued articles one at a time, as with summaries: the
// server says what is wanted and hands over text ready to be read; nothing is
// held here between runs.
func runSpeech(ctx context.Context, c *client, o speechOpts, logf func(string, ...any)) (done, failed int, err error) {
	if strings.TrimSpace(o.Command) == "" {
		return 0, 0, errors.New(`no text-to-speech command, e.g. deploy/tts/tiny-voca-speak`)
	}
	work, err := os.MkdirTemp("", "readroom-speak")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(work)

	type item struct {
		ID       int64  `json:"id"`
		Title    string `json:"title"`
		Language string `json:"language"`
		Text     string `json:"text"`
	}
	next := func(skip []string) (*item, error) {
		path := "/api/v1/audio/pending?limit=1"
		if len(skip) > 0 {
			path += "&exclude=" + strings.Join(skip, ",")
		}
		var queue struct {
			Entries []item `json:"entries"`
		}
		if err := c.do(ctx, "GET", path, nil, &queue); err != nil {
			return nil, err
		}
		if len(queue.Entries) == 0 {
			return nil, nil
		}
		return &queue.Entries[0], nil
	}

	var skip []string
	for o.Limit <= 0 || done+len(skip) < o.Limit {
		e, err := next(skip)
		if err != nil {
			return done, len(skip), err
		}
		if e == nil || slices.Contains(skip, fmt.Sprint(e.ID)) {
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
		voice := o.voiceFor(e.Language)
		audio, kind, err := synthesize(ctx, work, voice, e.Language, text)
		if ctx.Err() != nil {
			return done, len(skip), ctx.Err() // stopped, not failed: the article stays queued
		}
		if err != nil {
			// Say so on the article, where the owner is waiting, and go on to the next.
			skip = append(skip, fmt.Sprint(e.ID))
			logf("fail  #%d %s: %v", e.ID, e.Title, err)
			if err := c.do(ctx, "POST", fmt.Sprintf("/api/v1/entries/%d/audio/error", e.ID), map[string]string{"error": err.Error()}, nil); err != nil {
				return done, len(skip), err
			}
			continue
		}
		if err := c.upload(ctx, fmt.Sprintf("/api/v1/entries/%d/audio?voice=%s", e.ID, url.QueryEscape(voice.Voice)), kind, audio); err != nil {
			return done, len(skip), err
		}
		done++
		logf("done  #%d %s (%.1f MB, %.1fs)", e.ID, e.Title, float64(len(audio))/(1<<20), time.Since(start).Seconds())
	}
	return done, len(skip), nil
}

// synthesize runs a voice's text-to-speech command on text and returns the audio
// to upload with its content type. The command is told the text's language, when
// that is known, in READROOM_SPEECH_LANGUAGE, so one command can serve several.
func synthesize(ctx context.Context, work string, voice speechVoice, language, text string) ([]byte, string, error) {
	command, ctype, bitrate := voice.Command, voice.Type, voice.Bitrate
	raw := filepath.Join(work, "speech")
	out, err := os.Create(raw)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin = strings.NewReader(text)
	cmd.Env = append(os.Environ(), "READROOM_SPEECH_LANGUAGE="+language)
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
		enc := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", raw,
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
func (c *client) upload(ctx context.Context, path, ctype string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", c.base+path, bytes.NewReader(data))
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
