package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// mpvRPCDeadline bounds how long a single IPC request may take.
const mpvRPCDeadline = 800 * time.Millisecond

var errMpvNoFile = errors.New("no file playing")

// mpvClient talks to mpv's JSON IPC (one JSON object per line). A single
// buffered reader is kept for the whole connection so no reply bytes are
// lost between requests, and unsolicited event lines are skipped.
type mpvClient struct {
	conn   net.Conn
	r      *bufio.Reader
	nextID int64
}

func newMpvClient(conn net.Conn) *mpvClient {
	c := &mpvClient{conn: conn, r: bufio.NewReader(conn)}
	// Koushin only polls properties, so ask mpv not to push events at all.
	_, _ = c.command("disable_event", "all")
	return c
}

func (c *mpvClient) Close() error { return c.conn.Close() }

type mpvRequest struct {
	Command   []any `json:"command"`
	RequestID int64 `json:"request_id"`
}

type mpvResponse struct {
	Error     string `json:"error"`
	Data      any    `json:"data"`
	RequestID int64  `json:"request_id"`
	Event     string `json:"event"`
}

func (c *mpvClient) command(cmd ...any) (any, error) {
	c.nextID++
	id := c.nextID
	b, _ := json.Marshal(mpvRequest{Command: cmd, RequestID: id})
	b = append(b, '\n')

	_ = c.conn.SetWriteDeadline(time.Now().Add(mpvRPCDeadline))
	if _, err := c.conn.Write(b); err != nil {
		return nil, err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(mpvRPCDeadline))
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var resp mpvResponse
		if json.Unmarshal(line, &resp) != nil {
			continue // partial line left over from an earlier timeout
		}
		if resp.Event != "" {
			continue
		}
		if resp.RequestID != 0 && resp.RequestID != id {
			continue // late reply to an earlier request
		}
		if resp.Error != "success" {
			return nil, fmt.Errorf("mpv error: %s", resp.Error)
		}
		return resp.Data, nil
	}
}

func (c *mpvClient) alive() bool {
	_, err := c.command("get_property", "mpv-version")
	return err == nil
}

type mpvState struct {
	Path       string
	FileName   string
	MediaTitle string
	Duration   float64
	TimePos    float64
	Pause      bool
}

// extractStreamFilename handles direct-streaming links (Torbox, Real-Debrid,
// AllDebrid, and similar debrid/streaming services) where mpv's reported
// "path"/"filename" is just an opaque ID or token, e.g.:
//
//	82b584e3-2fc3-4e3c-9d38-14db419a8203?token=...&filename=Neon.Genesis.Evangelion.S01E01.1080p...mkv
//
// In that case the *real* media filename lives inside a query parameter
// rather than in the path itself. This pulls it out (URL-decoded) so the
// title/episode parser gets something meaningful to work with instead of a
// UUID. If no such query parameter exists, the input is returned unchanged
// (aside from stripping an unparsable query string, so raw tokens never leak
// into the parser).
func extractStreamFilename(nameOrPath string) string {
	s := strings.TrimSpace(nameOrPath)
	if s == "" {
		return s
	}

	qIdx := strings.IndexByte(s, '?')
	if qIdx < 0 {
		return s
	}

	values, err := url.ParseQuery(s[qIdx+1:])
	if err != nil {
		// Malformed query string - strip it rather than feed a raw token
		// (with "?" and friends) into the filename parser.
		return s[:qIdx]
	}

	for _, key := range []string{"filename", "file_name", "name"} {
		v := strings.TrimSpace(values.Get(key))
		if v == "" {
			continue
		}
		// url.ParseQuery already decodes percent-escapes, but some services
		// double-encode, so try one more pass defensively.
		if decoded, derr := url.QueryUnescape(v); derr == nil && decoded != "" {
			return decoded
		}
		return v
	}

	// No usable filename in the query string; strip it so at least no
	// token/signature garbage reaches the parser.
	return s[:qIdx]
}

// state reads the properties Koushin needs. Individual property errors are
// ignored (e.g. duration is unavailable while a file is loading); if neither
// a file name nor a title is available, errMpvNoFile is returned.
func (c *mpvClient) state() (mpvState, error) {
	var st mpvState
	str := func(name string) string {
		d, err := c.command("get_property", name)
		if err != nil {
			return ""
		}
		s, _ := d.(string)
		return s
	}
	num := func(name string) (float64, bool) {
		d, err := c.command("get_property", name)
		if err != nil {
			return 0, false
		}
		f, ok := d.(float64)
		return f, ok
	}

	st.Path = extractStreamFilename(str("path"))
	st.FileName = extractStreamFilename(str("filename"))
	st.MediaTitle = str("media-title")
	if f, ok := num("duration"); ok && f > 0 {
		st.Duration = f
	}
	if f, ok := num("time-pos"); ok && f >= 0 {
		st.TimePos = f
	}
	if d, err := c.command("get_property", "pause"); err == nil {
		st.Pause, _ = d.(bool)
	}
	if st.FileName == "" && st.MediaTitle == "" {
		return st, errMpvNoFile
	}
	return st, nil
}

func isPipeGone(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "pipe is being closed") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "cannot find the file") ||
		strings.Contains(s, "the system cannot find the file") ||
		strings.Contains(s, "eof")
}
