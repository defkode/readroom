package server

import (
	"testing"
	"time"

	"readroom/internal/store"
)

func TestReadWhen(t *testing.T) {
	if got := readWhen(nil); got != "Read" {
		t.Errorf("no read time: %q", got)
	}
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{
		{10 * time.Second, "Read just now"},
		{time.Minute + time.Second, "Read a minute ago"},
		{5 * time.Minute, "Read 5 minutes ago"},
		{61 * time.Minute, "Read an hour ago"},
		{3 * time.Hour, "Read 3 hours ago"},
		{30 * time.Hour, "Read yesterday"},
		{4 * 24 * time.Hour, "Read 4 days ago"},
		{15 * 24 * time.Hour, "Read 2 weeks ago"},
		{35 * 24 * time.Hour, "Read 5 weeks ago"},
		{70 * 24 * time.Hour, "Read 2 months ago"},
		{400 * 24 * time.Hour, "Read a year ago"},
	} {
		at := time.Now().Add(-c.ago).UTC().Format(store.TimeFormat)
		if got := readWhen(&at); got != c.want {
			t.Errorf("%v ago: got %q, want %q", c.ago, got, c.want)
		}
	}
}
