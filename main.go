package main

import (
	"bytes"
	_ "embed"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"net/url"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	habari "github.com/5rahim/habari"
	"github.com/getlantern/systray"
	"github.com/natefinch/npipe"
)

const mpvRPCDeadline = 800 * time.Millisecond

//go:embed koushin.ico
var iconICO []byte

func setTrayIcon() {
    if len(iconICO) > 0 {
        systray.SetIcon(iconICO)
        return
    }
    // Fallback: try to read from disk next to the exe
    if b, err := os.ReadFile("koushin.ico"); err == nil {
        systray.SetIcon(b)
    }
}

func logPath() string {
    return filepath.Join(appDataDir(), "koushin.log")
}
func logAppend(lines ...string) {
    f, err := os.OpenFile(logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
    if err != nil { return }
    defer f.Close()
    for _, s := range lines {
        ts := time.Now().Format("2006-01-02 15:04:05")
        _, _ = f.WriteString(ts + " " + s + "\n")
    }
}



// ─────────────────────────────────────────────────────────────
// AniList OAuth config (EDIT ME: set your Client ID + redirect)
// ─────────────────────────────────────────────────────────────
const (
	anilistClientID = "31833" // <-- put your AniList Client ID here
	redirectPort    = 45123                     // must match the redirect URI you set in AniList app
)

var (
	anilistAuthURL  = "https://anilist.co/api/v2/oauth/authorize"
	anilistTokenURL = "https://anilist.co/api/v2/oauth/token"
	anilistGQLURL   = "https://graphql.anilist.co"
)

type Config struct {
	DiscordAppID string
	MpvPipe      string
	PollInterval time.Duration
	UserAgent    string
	SmallImage   string
}

func loadConfig() Config {
	appID := "1434412611411120198" // your Discord App ID
	pipe := os.Getenv("MPV_PIPE")
	if pipe == "" {
		pipe = `\\.\pipe\mpv-pipe`
	}
	poll := 1000 * time.Millisecond
	if v := os.Getenv("POLL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 200 {
			poll = time.Duration(ms) * time.Millisecond
		}
	}
	ua := os.Getenv("HTTP_USER_AGENT")
	if ua == "" {
		ua = "koushin/1.2 (+https://anilist.co)"
	}
	small := os.Getenv("SMALL_IMAGE_KEY")
	return Config{
		DiscordAppID: appID,
		MpvPipe:      pipe,
		PollInterval: poll,
		UserAgent:    ua,
		SmallImage:   small,
	}
}

/* ====================== mpv IPC (unchanged) ====================== */

type mpvRequest struct{ Command []any `json:"command"` }
type mpvResponse struct {
	Error string      `json:"error"`
	Data  interface{} `json:"data,omitempty"`
}

func mpvSend(conn net.Conn, cmd ...any) (interface{}, error) {
	req := mpvRequest{Command: cmd}
	b, _ := json.Marshal(req)
	b = append(b, '\n')

	_ = conn.SetWriteDeadline(time.Now().Add(mpvRPCDeadline))
	if _, err := conn.Write(b); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(mpvRPCDeadline))
	dec := json.NewDecoder(conn)
	var resp mpvResponse
	if err := dec.Decode(&resp); err != nil {
		return nil, err
	}
	if resp.Error != "success" {
		return nil, fmt.Errorf("mpv error: %s", resp.Error)
	}
	return resp.Data, nil
}

type mpvState struct {
	FileName   string
	MediaTitle string
	Duration   float64
	TimePos    float64
	TimeRem    float64
	Pause      bool
}

func asFloat(x any) (float64, bool) {
	switch v := x.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func queryMpvState(conn net.Conn) (mpvState, error) {
	var st mpvState
	if d, err := mpvSend(conn, "get_property", "filename"); err == nil {
		if s, ok := d.(string); ok { st.FileName = s }
	}
	if d, err := mpvSend(conn, "get_property", "media-title"); err == nil {
		if s, ok := d.(string); ok { st.MediaTitle = s }
	}
	if d, err := mpvSend(conn, "get_property", "duration"); err == nil {
		if f, ok := asFloat(d); ok && f > 0 { st.Duration = f }
	}
	if d, err := mpvSend(conn, "get_property", "time-pos"); err == nil {
		if f, ok := asFloat(d); ok && f >= 0 { st.TimePos = f }
	}
	if d, err := mpvSend(conn, "get_property", "time-remaining"); err == nil {
		if f, ok := asFloat(d); ok && f >= 0 { st.TimeRem = f }
	}
	if d, err := mpvSend(conn, "get_property", "pause"); err == nil {
		if b, ok := d.(bool); ok { st.Pause = b }
	}
	if st.FileName == "" && st.MediaTitle == "" {
		return st, errors.New("no file playing")
	}
	return st, nil
}

/* ==================== AniList search (year-aware) ==================== */

type mediaLite struct {
	ID    int `json:"id"`
	Title struct {
		Romaji  string `json:"romaji"`
		English string `json:"english"`
		Native  string `json:"native"`
	} `json:"title"`
	CoverImage struct {
		Large string `json:"large"`
	} `json:"coverImage"`
	StartDate struct {
		Year int `json:"year"`
	} `json:"startDate"`
}

type anilistResp struct {
	Data struct {
		Page struct {
			Media []mediaLite `json:"media"`
		} `json:"Page"`
	} `json:"data"`
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

var (
	reParensYear = regexp.MustCompile(`\s*\((19|20)\d{2}\)`)
	reEpTail     = regexp.MustCompile(`\s*[-–—]\s*(?:ep|episode)?\s*\d{1,4}\s*$`)
	reBrackets   = regexp.MustCompile(`\s*[\[\(][^\]\)]*[\]\)]`)
	reMultiSpace = regexp.MustCompile(`\s{2,}`)
	reAnyYear    = regexp.MustCompile(`\b(19|20)\d{2}\b`)
)

func parseYearString(s string) int {
	if s == "" { return 0 }
	if m := reAnyYear.FindString(s); m != "" {
		if y, err := strconv.Atoi(m); err == nil { return y }
	}
	return 0
}

func wantYearFrom(md *habari.Metadata, key, mediaTitle string) int {
	if y, err := strconv.Atoi(strings.TrimSpace(md.Year)); err == nil && y > 1900 { return y }
	if y := parseYearString(key); y > 0 { return y }
	if y := parseYearString(mediaTitle); y > 0 { return y }
	return 0
}

func cleanTitleForSearch(s string) string {
	s = strings.TrimSpace(s)
	s = reParensYear.ReplaceAllString(s, "")
	s = reEpTail.ReplaceAllString(s, "")
	s = reBrackets.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, ".", " ")
	s = reMultiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func pickBest(ms []mediaLite, wantYear int) (n string, c string, i int) {
	if len(ms) == 0 { return "", "", 0 }
	if wantYear > 0 {
		for _, m := range ms {
			if m.StartDate.Year == wantYear {
				n = strings.TrimSpace(m.Title.English)
				if n == "" { n = firstNonEmpty(m.Title.Romaji, m.Title.Native) }
				return n, m.CoverImage.Large, m.ID
			}
		}
	}
	m := ms[0]
	n = strings.TrimSpace(m.Title.English)
	if n == "" { n = firstNonEmpty(m.Title.Romaji, m.Title.Native) }
	return n, m.CoverImage.Large, m.ID
}

func findAniList(ctx context.Context, rawTitle string, wantYear int, ua string) (name, coverURL string, id int, err error) {
	cands := []string{ cleanTitleForSearch(rawTitle), rawTitle }
	const q = `
query($search: String) {
  Page(perPage: 10) {
    media(search: $search, type: ANIME, sort: [SEARCH_MATCH, START_DATE_DESC]) {
      id
      title { romaji english native }
      coverImage { large }
      startDate { year }
    }
  }
}`
	for _, title := range cands {
		body := map[string]any{"query": q, "variables": map[string]any{"search": title}}
		bs, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
		req.Header.Set("Content-Type", "application/json")
		if ua != "" { req.Header.Set("User-Agent", ua) }
		resp, rerr := httpClient.Do(req)
		if rerr != nil { err = rerr; continue }
		func() {
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				err = fmt.Errorf("anilist http %d: %s", resp.StatusCode, string(b)); return
			}
			var ar anilistResp
			if derr := json.NewDecoder(resp.Body).Decode(&ar); derr != nil { err = derr; return }
			ms := ar.Data.Page.Media
			if len(ms) == 0 { err = errors.New("no match on AniList"); return }
			name, coverURL, id = pickBest(ms, wantYear); err = nil
		}()
		if err == nil && id != 0 { return }
	}
	if err == nil { err = errors.New("no match on AniList") }
	return
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals { if strings.TrimSpace(v) != "" { return v } }
	return ""
}
func anilistURL(id int) string {
	if id <= 0 { return "" }
	return fmt.Sprintf("https://anilist.co/anime/%d", id)
}

/* ===================== Discord native IPC (same) ===================== */

const (
	opHandshake = 0
	opFrame     = 1
	opClose     = 2
)

type discordIPC struct {
	conn io.ReadWriteCloser
	mu   sync.Mutex
}

func connectDiscordIPC(appID string) (*discordIPC, error) {
	var conn io.ReadWriteCloser
	var err error
	for i := 0; i < 10; i++ {
		path := fmt.Sprintf(`\\.\pipe\discord-ipc-%d`, i)
		conn, err = npipe.DialTimeout(path, 2*time.Second)
		if err == nil { break }
	}
	if conn == nil { return nil, errors.New("could not connect to any discord-ipc pipe") }
	ipc := &discordIPC{conn: conn}
	hello := map[string]any{"v": 1, "client_id": appID}
	if err := ipc.write(opHandshake, hello); err != nil {
		conn.Close(); return nil, fmt.Errorf("handshake failed: %w", err)
	}
	return ipc, nil
}

func (d *discordIPC) write(code uint32, payload any) error {
	d.mu.Lock(); defer d.mu.Unlock()
	data, err := json.Marshal(payload); if err != nil { return err }
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:4], code)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(data)))
	if _, err := d.conn.Write(hdr); err != nil { return err }
	_, err = d.conn.Write(data)
	return err
}
func (d *discordIPC) close() error {
	_ = d.write(opClose, map[string]any{}); return d.conn.Close()
}
func (d *discordIPC) setActivity(appID string, activity map[string]any) error {
	args := map[string]any{"pid": os.Getpid(), "activity": activity}
	envelope := map[string]any{
		"cmd": "SET_ACTIVITY", "args": args,
		"nonce": fmt.Sprintf("%d-%d", time.Now().UnixNano(), mrand.Int63()),
	}
	return d.write(opFrame, envelope)
}

/* ========================= Progress smoothing ========================= */

type progSmooth struct {
	mu          sync.Mutex
	lastQueryAt time.Time
	lastPos     float64
	duration    float64
	paused      bool
	initialized bool
}
func (p *progSmooth) updateFromMPV(pos, dur float64, paused bool) {
	p.mu.Lock(); defer p.mu.Unlock()
	p.lastQueryAt = time.Now(); p.lastPos = pos; p.duration = dur; p.paused = paused; p.initialized = true
}
func (p *progSmooth) estimate() (pos, dur float64, paused bool) {
	p.mu.Lock(); defer p.mu.Unlock()
	pos, dur, paused = p.lastPos, p.duration, p.paused
	if !p.initialized || p.lastQueryAt.IsZero() { return }
	if !paused && dur > 0 {
		elapsed := time.Since(p.lastQueryAt).Seconds()
		pos = p.lastPos + elapsed
		if pos > dur { pos = dur }
	}
	return
}

/* ====================== Presence helpers (yours) ====================== */

func tsRange(now time.Time, cur, dur float64, paused bool) map[string]any {
	if paused || dur <= 0 || cur < 0 || cur > dur { return nil }
	start := now.Add(-time.Duration(cur) * time.Second).Unix()
	end := now.Add(time.Duration(dur-cur) * time.Second).Unix()
	return map[string]any{"start": start, "end": end}
}

func buildActivity(title, episode, _clock, coverURL, _smallKey, _aniURL string, cur, dur float64, paused bool) map[string]any {
	details := title
	epText := "Episode —"
	if strings.TrimSpace(episode) != "" { epText = "Episode " + episode }
	state := epText
	if paused { state = "Paused — " + epText }

	assets := map[string]any{
		"large_image": coverURL,
		"large_text":  title,
	}
	if _aniURL != "" { assets["large_url"] = _aniURL }

	act := map[string]any{
		"name":    details,
		"details": details,
		"state":   state,
		"type":    3,
		"assets":  assets,
	}
	if dur > 0 {
		if tr := tsRange(time.Now(), cur, dur, paused); tr != nil {
			act["timestamps"] = tr
		}
	}
	return act
}

/* =================== Episode/title parsing (same) =================== */

type presenceCache struct {
	mu         sync.Mutex
	lastFile   string
	lastAni    string
	lastEp     string
	lastCover  string
	lastAniID  int
	startEpoch time.Time
}

var (
	reEGeneric = regexp.MustCompile(`(?i)\b(?:ep|eps|episode)\s*[-_. ]*\s*(\d{1,4})\b`)
	reDashNum  = regexp.MustCompile(`(?:^|[-_. \[\(])(\d{1,4})(?:v\d)?(?:[-_. \]\)]|$)`)
	reSxxExx   = regexp.MustCompile(`(?i)\bS\d{1,2}E(\d{1,3})\b`)
)

func pickEpisode(md *habari.Metadata, fallbackTitle string) (titleOut string, ep string) {
	titleOut = firstNonEmpty(md.Title, md.FormattedTitle, fallbackTitle)
	for _, arr := range [][]string{md.EpisodeNumber, md.EpisodeNumberAlt, md.OtherEpisodeNumber} {
		if len(arr) > 0 && strings.TrimSpace(arr[0]) != "" {
			ep = strings.TrimLeft(arr[0], "0"); if ep == "" { ep = "0" }
			return
		}
	}
	if ep == "" { ep = guessEpisodeFromString(md.FileName) }
	if ep == "" { ep = guessEpisodeFromString(fallbackTitle) }
	return
}

func guessEpisodeFromString(s string) string {
	if s == "" { return "" }
	if m := reEGeneric.FindStringSubmatch(s); len(m) == 2 { return strings.TrimLeft(m[1], "0") }
	if m := reSxxExx.FindStringSubmatch(s); len(m) == 2 { return strings.TrimLeft(m[1], "0") }
	if m := reDashNum.FindStringSubmatch(s); len(m) == 2 { return strings.TrimLeft(m[1], "0") }
	return ""
}

/* ========================= Tray (systray) ========================= */

type trayState struct {
	mu      sync.Mutex
	playing bool
	title   string
	ep      string
}
func (t *trayState) setIdle()  { t.mu.Lock(); t.playing=false; t.title=""; t.ep=""; t.mu.Unlock(); t.refresh() }
func (t *trayState) setNow(title, ep string) { t.mu.Lock(); t.playing=true; t.title=title; t.ep=ep; t.mu.Unlock(); t.refresh() }

const tooltipMax = 120
func truncateRunes(s string, n int) string { r := []rune(s); if len(r)<=n {return s}; if n<=1 {return "…"}; return string(r[:n-1])+"…" }
func buildTooltip(title, ep string) string {
	if strings.TrimSpace(title) == "" { return "Koushin" }
	epText := "Ep —"; if strings.TrimSpace(ep) != "" { epText = "Ep " + ep }
	base := fmt.Sprintf("Koushin — %s — ", epText)
	avail := tooltipMax - len([]rune(base)); if avail < 0 { avail = 0 }
	return base + truncateRunes(title, avail)
}
func (t *trayState) refresh() {
	t.mu.Lock(); defer t.mu.Unlock()
	if !t.playing { systray.SetTooltip("Koushin"); return }
	systray.SetTooltip(buildTooltip(t.title, t.ep))
}

var (
	globalTray   = &trayState{}
	menuQuit     *systray.MenuItem
	menuLogin    *systray.MenuItem
	menuLogout   *systray.MenuItem
)

func refreshAuthMenu() {
    if store.AccessToken == "" {
        menuLogin.SetTitle("Sign in to AniList…")
        menuLogin.Enable()
        menuLogout.Disable()
    } else {
        title := "AniList: Signed in"
        if store.Username != "" {
            title = "Signed in as @" + store.Username
        }
        menuLogin.SetTitle(title)
        menuLogin.Disable()
        menuLogout.Enable()
    }
}

/* ===================== AniList OAuth storage ===================== */

type authStore struct {
	Path        string
	AccessToken string
	Username    string
	UserID      int
	mu          sync.RWMutex
}

func appDataDir() string {
	base, _ := os.UserConfigDir()
	if base == "" {
		base = filepath.Join(os.TempDir(), "Koushin")
	} else {
		base = filepath.Join(base, "Koushin")
	}
	_ = os.MkdirAll(base, 0700)
	return base
}
func (a *authStore) file() string { return filepath.Join(a.Path, "auth.json") }

func (a *authStore) Load() {
	a.mu.Lock(); defer a.mu.Unlock()
	b, err := os.ReadFile(a.file())
	if err != nil { return }
	var tmp struct {
		AccessToken string `json:"access_token"`
		Username    string `json:"username"`
		UserID      int    `json:"user_id"`
	}
	if json.Unmarshal(b, &tmp) == nil {
		a.AccessToken = tmp.AccessToken; a.Username = tmp.Username; a.UserID = tmp.UserID
	}
}
func (a *authStore) Save() {
	a.mu.RLock()
	data := struct {
		AccessToken string `json:"access_token"`
		Username    string `json:"username"`
		UserID      int    `json:"user_id"`
	}{a.AccessToken, a.Username, a.UserID}
	a.mu.RUnlock()
	b, _ := json.MarshalIndent(data, "", "  ")
	_ = os.WriteFile(a.file(), b, 0600)
}
func (a *authStore) Clear() {
	a.mu.Lock(); a.AccessToken=""; a.Username=""; a.UserID=0; a.mu.Unlock()
	_ = os.Remove(a.file())
}

var store = &authStore{ Path: appDataDir() }

/* ====================== AniList OAuth (PKCE) ====================== */

func randBytes(n int) []byte { b := make([]byte, n); _, _ = rand.Read(b); return b }
func b64UrlNoPad(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func sha256B64(s string) string { h := sha256.Sum256([]byte(s)); return base64.RawURLEncoding.EncodeToString(h[:]) }

func startLocalCallbackServer(ctx context.Context, ch chan<- string) (func(), error) {
	mux := http.NewServeMux()
	srv := &http.Server{ Addr: fmt.Sprintf("127.0.0.1:%d", redirectPort), Handler: mux }
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		logAppend("oauth: /callback hit, code length=", fmt.Sprint(len(code)))
		_, _ = io.WriteString(w, "You may close this window and return to Koushin.")
		go func(){ ch <- code }()
	})
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil { return nil, err }
	go func(){ _ = srv.Serve(ln) }()
	cleanup := func(){ _ = srv.Shutdown(context.Background()) }
	return cleanup, nil
}

func openBrowser(u string) {
    _ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}

// Use os/exec to launch the browser helper
func execCommand(name string, args ...string) error {
    cmd := exec.Command(name, args...)
    // non-blocking launch; we don’t care about the output
    return cmd.Start()
}


// We’ll avoid bringing in os/exec; just use ShellExecute via rundll32 on Windows above.
// (If you prefer, re-add "os/exec" and a standard exec.Command call.)

// Implicit flow with NO redirect_uri: user copies the access token from AniList and pastes it.
// Requires: anilistClientID, httpClient, store, whoAmI, openBrowser, logAppend.
func oauthLogin(ctx context.Context) error {
    if anilistClientID == "" || anilistClientID == "YOUR_ANILIST_CLIENT_ID" {
        return errors.New("set anilistClientID in the source")
    }

    // Local entry page (only for pasting token; not used as a redirect)
    const localLoginAddr = "127.0.0.1:45124"
    enterURL := "http://" + localLoginAddr + "/enter"

    tokenCh := make(chan string, 1)
    srv := &http.Server{Addr: localLoginAddr}
    mux := http.NewServeMux()

    // /enter page: user pastes the token (or full URL) manually
    mux.HandleFunc("/enter", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Koushin · AniList Login</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5">
<h2>Paste your AniList access token</h2>
<ol>
  <li>In the AniList page you just opened, authorize Koushin.</li>
  <li>Copy the <b>Access Token</b> shown by AniList (or the full URL containing <code>#access_token=...</code>).</li>
  <li>Paste it below and submit.</li>
</ol>
<form method="POST" action="/submit" onsubmit="return true">
  <textarea name="blob" style="width:100%;height:110px;font-family:ui-monospace,monospace" placeholder="access_token value or full URL with #access_token=..."></textarea>
  <div style="margin-top:12px">
    <button type="submit" style="padding:8px 14px;font-size:15px">Submit</button>
  </div>
</form>
<p id="status"></p>
</body></html>`)
    })

    // /submit accepts either a raw token or a full URL/fragment; extracts access_token
    mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
        if r.Method != "POST" {
            http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
            return
        }
        _ = r.ParseForm()
        raw := strings.TrimSpace(r.Form.Get("blob"))
        if raw == "" {
            http.Error(w, "empty submission", http.StatusBadRequest)
            return
        }
        tok := extractAccessToken(raw)
        if tok == "" {
            http.Error(w, "no access_token found", http.StatusBadRequest)
            return
        }
        go func(){ tokenCh <- tok }()
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        _, _ = w.Write([]byte("OK — you can close this tab."))
    })

    srv.Handler = mux
    go func() { _ = srv.ListenAndServe() }()
    defer func() {
        c2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
        _ = srv.Shutdown(c2)
        cancel()
    }()

    // Open AniList authorize (NO redirect_uri)
    authURL := "https://anilist.co/api/v2/oauth/authorize?client_id=" + url.QueryEscape(anilistClientID) + "&response_type=token"
    logAppend("oauth(implicit-no-redirect): opening ", authURL)
    openBrowser(authURL)

    // Also open our entry page for pasting
    openBrowser(enterURL)

    // Wait for token (or cancel)
    var accessToken string
    select {
    case <-ctx.Done():
        return ctx.Err()
    case accessToken = <-tokenCh:
        if strings.TrimSpace(accessToken) == "" {
            return errors.New("empty access token")
        }
        logAppend("oauth(implicit-no-redirect): got token len=", fmt.Sprint(len(accessToken)))
    }

    // Fetch Viewer to store username/id
    name, uid, whoErr := whoAmI(ctx, accessToken)
    if whoErr != nil {
        logAppend("oauth: whoAmI error: ", whoErr.Error())
    }

    store.mu.Lock()
    store.AccessToken = accessToken
    store.Username = name
    store.UserID = uid
    store.mu.Unlock()
    store.Save()

    logAppend("oauth: success; logged in as ", name, " (id ", fmt.Sprint(uid), ")")
    return nil
}


func extractAccessToken(s string) string {
    s = strings.TrimSpace(s)
    if s == "" { return "" }

    // Full URL with fragment?
    if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
        u, err := url.Parse(s)
        if err == nil {
            frag := strings.TrimPrefix(u.Fragment, "#")
            vals, _ := url.ParseQuery(frag)
            if t := vals.Get("access_token"); t != "" {
                return t
            }
        }
    }
    // Fragment-only paste
    if strings.Contains(s, "access_token=") {
        frag := strings.TrimPrefix(s, "#")
        vals, _ := url.ParseQuery(frag)
        if t := vals.Get("access_token"); t != "" {
            return t
        }
    }
    // Raw token heuristic
    if len(s) > 40 && !strings.ContainsAny(s, " \n\t") {
        return s
    }
    return ""
}





func urlQueryEscape(s string) string { // minimalist; real code: use url.QueryEscape
	r := strings.NewReplacer(" ", "%20", ":", "%3A", "/", "%2F", "?", "%3F", "=", "%3D", "&", "%26", "+", "%2B", "#", "%23")
	return r.Replace(s)
}

func whoAmI(ctx context.Context, token string) (username string, userID int, err error) {
	const q = `query{ Viewer { id name } }`
	body := map[string]any{"query": q}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req); if err != nil { return }
	defer resp.Body.Close()
	if resp.StatusCode != 200 { b, _ := io.ReadAll(resp.Body); return "", 0, fmt.Errorf("whoAmI http %d: %s", resp.StatusCode, string(b)) }
	var out struct{ Data struct{ Viewer struct{ ID int `json:"id"`; Name string `json:"name"` } `json:"Viewer"` } `json:"data"` }
	if json.NewDecoder(resp.Body).Decode(&out) == nil {
		return out.Data.Viewer.Name, out.Data.Viewer.ID, nil
	}
	return "", 0, errors.New("decode viewer failed")
}

func saveProgress(ctx context.Context, token string, mediaID int, episode int) error {
	const m = `
mutation($mediaId:Int, $progress:Int) {
  SaveMediaListEntry(mediaId:$mediaId, progress:$progress) { id status progress }
}`
	vars := map[string]any{"mediaId": mediaID, "progress": episode}
	body := map[string]any{"query": m, "variables": vars}
	bs, _ := json.Marshal(body)

	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req); if err != nil { return err }
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("SaveMediaListEntry http %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

/* ========================= Tray lifecycle ========================= */

func onReadyTray(ctx context.Context, cancel context.CancelFunc) {
    setTrayIcon()
    systray.SetTooltip("Koushin")

    menuLogin  = systray.AddMenuItem("Sign in to AniList…", "Authenticate this device with AniList")
    menuLogout = systray.AddMenuItem("Sign out of AniList", "Forget saved AniList token")
    systray.AddSeparator()
    menuQuit   = systray.AddMenuItem("Quit", "Exit Koushin")

    refreshAuthMenu() // <<< reflect current token

    go func() {
        for {
            select {
			case <-menuLogin.ClickedCh:
				c, cancel2 := context.WithTimeout(ctx, 5*time.Minute)
				err := oauthLogin(c)
				cancel2()

				if err != nil {
					fmt.Println("AniList login failed:", err)
					logAppend("AniList login failed:", err.Error())
				} else {
					fmt.Println("AniList login succeeded")
				}

				// Reload what we saved (defensive)
				store.Load()
				refreshAuthMenu()


			case <-menuLogout.ClickedCh:
				store.Clear()
				refreshAuthMenu()


            case <-menuQuit.ClickedCh:
                cancel(); return
            case <-ctx.Done():
                return
            }
        }
    }()
}


func runWithTray(mainfn func(context.Context), onExit func()) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() { systray.Run(func(){ onReadyTray(ctx, cancel) }, func(){}) }()
	go func() { mainfn(ctx); cancel() }()
	<-ctx.Done()
	if onExit != nil { onExit() }
	systray.Quit()
}

/* ============================ Main ============================ */

func main() {
    cfg := loadConfig()
    store.Load()               // load saved token BEFORE tray shown
    runWithTray(func(ctx context.Context){ run(ctx, cfg) }, nil)
}

/* ==================== Core loop with 80% update ==================== */

func secondsToStamp(s float64) (min, sec int) { if s < 0 { return 0,0 }; return int(s)/60, int(s)%60 }
func formatClock(cur, dur float64) string {
	cm, cs := secondsToStamp(cur)
	if dur <= 0 { return fmt.Sprintf("%d:%02d / —:—", cm, cs) }
	dm, ds := secondsToStamp(dur)
	return fmt.Sprintf("%d:%02d / %d:%02d", cm, cs, dm, ds)
}

func fallbackTitleFrom(st mpvState) string {
	if t := strings.TrimSpace(st.MediaTitle); t != "" { return t }
	base := filepath.Base(strings.TrimSpace(st.FileName))
	if i := strings.LastIndexByte(base, '.'); i >= 0 { base = base[:i] }
	return base
}

func run(ctx context.Context, cfg Config) {
	var discord *discordIPC
	var err error
	if cfg.DiscordAppID != "MISSING_APP_ID" {
		for {
			select { case <-ctx.Done(): return; default: }
			discord, err = connectDiscordIPC(cfg.DiscordAppID)
			if err != nil { time.Sleep(1500*time.Millisecond); continue }
			break
		}
	}
	defer func() {
		if discord != nil { _ = discord.setActivity(cfg.DiscordAppID, nil); _ = discord.close() }
	}()

	for {
		select { case <-ctx.Done(): return; default: }
		mpvConn, err := npipe.DialTimeout(cfg.MpvPipe, 2*time.Second)
		if err != nil {
			if discord != nil { _ = discord.setActivity(cfg.DiscordAppID, nil) }
			globalTray.setIdle()
			time.Sleep(1500*time.Millisecond)
			continue
		}
		runLoop(ctx, cfg, mpvConn, discord)
		time.Sleep(1 * time.Second)
	}
}

func runLoop(ctx context.Context, cfg Config, conn net.Conn, discord *discordIPC) {
	defer conn.Close()

	cache := &presenceCache{}
	smooth := &progSmooth{}
	mpvTicker := time.NewTicker(cfg.PollInterval)
	uiTicker  := time.NewTicker(1 * time.Second)
	defer mpvTicker.Stop(); defer uiTicker.Stop()

	var currentFileKey string
	playing := false
	ready   := false

	setPresence := func(details map[string]any) {
		if discord != nil { _ = discord.setActivity(cfg.DiscordAppID, details) }
	}
	pushEstimated := func() {
		if !playing { return }
		cache.mu.Lock()
		aname, ep, cover, aniID := cache.lastAni, cache.lastEp, cache.lastCover, cache.lastAniID
		cache.mu.Unlock()

		var pos, dur float64; var paused bool
		if ready { pos, dur, paused = smooth.estimate() } else { _, dur, paused = smooth.estimate(); pos = 0 }

		act := buildActivity(aname, ep, "", cover, cfg.SmallImage, anilistURL(aniID), pos, dur, paused)
		if !ready { delete(act, "timestamps") }
		setPresence(act)
		globalTray.setNow(aname, ep)
	}

	// already-reported tracker: mediaID:episode -> true
	reported := make(map[string]bool)

	for {
		select {
		case <-ctx.Done():
			return

		case <-mpvTicker.C:
			st, err := queryMpvState(conn)
			if err != nil {
				if strings.Contains(err.Error(), "no file playing") {
					playing=false; ready=false
					if discord != nil { _ = discord.setActivity(cfg.DiscordAppID, nil) }
					globalTray.setIdle()
					continue
				}
				if discord != nil { _ = discord.setActivity(cfg.DiscordAppID, nil) }
				globalTray.setIdle()
				return
			}
			playing = true
			if st.Duration > 0 { ready = true }
			smooth.updateFromMPV(st.TimePos, st.Duration, st.Pause)

			key := st.FileName; if key == "" { key = st.MediaTitle }
			if key != "" && key != currentFileKey {
				currentFileKey = key; ready = st.Duration > 0
				md := habari.Parse(key)
				title, ep := pickEpisode(md, fallbackTitleFrom(st))
				wantYear := wantYearFrom(md, key, st.MediaTitle)

				qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
				aname, cover, aid, aerr := findAniList(qctx, title, wantYear, cfg.UserAgent)
				cancel()
				if aerr != nil { aname = title; cover = "" }

				cache.mu.Lock()
				cache.lastFile = key; cache.lastAni = aname; cache.lastEp = ep
				cache.lastCover = cover; cache.lastAniID = aid
				cache.startEpoch = time.Now().Add(-time.Duration(st.TimePos) * time.Second)
				cache.mu.Unlock()

				// clear reported flags when switching media
				reported = make(map[string]bool)

				pushEstimated()
				continue
			}

			// ---- NEW: Auto update at 80% ----
			if ready && !st.Pause && st.Duration > 0 {
				ratio := st.TimePos / st.Duration
				if ratio >= 0.80 {
					cache.mu.Lock()
					aid := cache.lastAniID
					epStr := cache.lastEp
					cache.mu.Unlock()
					if aid > 0 && epStr != "" {
						if epNum, err := strconv.Atoi(epStr); err == nil && epNum > 0 {
							key := fmt.Sprintf("%d:%d", aid, epNum)
							if !reported[key] && store.AccessToken != "" {
								c, cancel := context.WithTimeout(ctx, 6*time.Second)
								err := saveProgress(c, store.AccessToken, aid, epNum)
								cancel()
								if err == nil {
									reported[key] = true
								}
							}
						}
					}
				}
			}

		case <-uiTicker.C:
			if playing { pushEstimated() }
		}
	}
}
