//go:build simul

// Simulwatching (host/join a watch session over TCP) is DISABLED by default.
// It needs port forwarding to work reliably, so it is excluded from normal
// builds. Build with `-tags simul` to bring it back; simul_off.go provides
// the no-op stand-ins used otherwise.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"
	natpmp "github.com/jackpal/go-nat-pmp"

	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/dcps/internetgateway2"
)

type simulMode string

const (
	simulModeOff  simulMode = "off"
	simulModeHost simulMode = "host"
	simulModeJoin simulMode = "join"
)

type simulMessage struct {
	Type string          `json:"type"`
	TS   int64           `json:"ts"`
	Data json.RawMessage `json:"data,omitempty"`
}

type simulHello struct {
	Code string `json:"code"`
}

type simulParticipants struct {
	Participants int `json:"participants"`
}

type simulHelloOK struct {
	Role      string `json:"role"`
	HostName  string `json:"host_name,omitempty"`
	HostVer   string `json:"host_ver,omitempty"`
	Joiners   int    `json:"joiners,omitempty"`
	InviteStr string `json:"invite,omitempty"`
}

type simulState struct {
	Anime      string  `json:"anime"`
	Episode    string  `json:"episode"`
	AniID      int     `json:"ani_id"`
	TotalEps   int     `json:"total_eps"`
	CoverURL   string  `json:"cover_url"`
	Rewatching bool    `json:"rewatching"`
	Paused     bool    `json:"paused"`
	Pos        float64 `json:"pos"`
	Dur        float64 `json:"dur"`
	Ready      bool    `json:"ready"`
	UpdatedAt  int64   `json:"updated_at"`
	HostFile   string  `json:"host_file,omitempty"`
	HostSeason int     `json:"host_season,omitempty"`
}

type simulSyncEvent struct {
	AniID     int   `json:"ani_id"`
	Episode   int   `json:"episode"`
	TotalEps  int   `json:"total_eps"`
	AtUnixSec int64 `json:"at_unix_sec"`
}

type simulConfig struct {
	Port         int
	AutoPortMap  bool
	JoinerSyncAL bool
}

type simulRuntime struct {
	mu sync.Mutex

	mode simulMode

	ln         net.Listener
	code       string
	clients    map[net.Conn]struct{}
	inviteText string
	portMapOff func()
	hostState  simulState

	conn      net.Conn
	lastState simulState
	status    string

	participants int

	cfg simulConfig
}

var simul = simulRuntime{mode: simulModeOff, status: "Off"}

func simulLoadSettingsFromStore() {
	store.mu.RLock()
	port := store.SimulPort
	auto := store.SimulAutoUPnP
	joinSync := store.SimulJoinSync
	store.mu.RUnlock()
	if !validateSimulPort(port) {
		port = defaultSimulPort()
	}
	simul.mu.Lock()
	simul.cfg = simulConfig{Port: port, AutoPortMap: auto, JoinerSyncAL: joinSync}
	simul.mu.Unlock()
}

func simulSetStatusLocked(status string) {
	simul.status = status
	if menuSimulStatus != nil {
		menuSimulStatus.SetTitle("Status: " + status)
	}
}

func simulSetParticipantsLocked(n int) {
	if n < 0 {
		n = 0
	}
	simul.participants = n
}

func simulRefreshTray() {
	simul.mu.Lock()
	mode := simul.mode
	invite := simul.inviteText
	participants := simul.participants
	simul.mu.Unlock()

	if menuSimulStatus != nil {
		status := "Off"
		switch mode {
		case simulModeHost:
			if participants <= 0 {
				participants = 1
			}
			status = fmt.Sprintf("Host (%d participants)", participants)
		case simulModeJoin:
			if participants <= 0 {
				participants = 1
			}
			status = fmt.Sprintf("Joined (%d participants)", participants)
		default:
			status = "Off"
		}
		menuSimulStatus.SetTitle("Status: " + status)
		if invite != "" {
			menuSimulStatus.SetTooltip(invite)
		}
	}
	if menuSimulHost != nil {
		if mode == simulModeOff {
			menuSimulHost.Enable()
		} else {
			menuSimulHost.Disable()
		}
	}
	if menuSimulJoin != nil {
		if mode == simulModeOff {
			menuSimulJoin.Enable()
		} else {
			menuSimulJoin.Disable()
		}
	}
	if menuSimulStop != nil {
		if mode == simulModeOff {
			menuSimulStop.Disable()
		} else {
			menuSimulStop.Enable()
		}
	}
	if menuSimulJoinSync != nil {
		if mode == simulModeJoin {
			menuSimulJoinSync.Enable()
		} else {
			menuSimulJoinSync.Disable()
		}
		store.mu.RLock()
		on := store.SimulJoinSync
		store.mu.RUnlock()
		if on {
			menuSimulJoinSync.Check()
		} else {
			menuSimulJoinSync.Uncheck()
		}
	}
}

func simulStopLocked() {
	if simul.portMapOff != nil {
		simul.portMapOff()
		simul.portMapOff = nil
	}
	if simul.conn != nil {
		_ = simul.conn.Close()
		simul.conn = nil
	}
	if simul.ln != nil {
		_ = simul.ln.Close()
		simul.ln = nil
	}
	for c := range simul.clients {
		_ = c.Close()
	}
	simul.clients = nil
	simul.code = ""
	simul.inviteText = ""
	simul.hostState = simulState{}
	simul.lastState = simulState{}
	simul.mode = simulModeOff
	simulSetParticipantsLocked(0)
	simulSetStatusLocked("Off")
}

func simulStop() {
	simul.mu.Lock()
	simulStopLocked()
	simul.mu.Unlock()
	simulRefreshTray()
}

func simulBroadcastToClients(typ string, payload any) {
	simul.mu.Lock()
	if simul.mode != simulModeHost || len(simul.clients) == 0 {
		simul.mu.Unlock()
		return
	}
	clients := make([]net.Conn, 0, len(simul.clients))
	for c := range simul.clients {
		clients = append(clients, c)
	}
	simul.mu.Unlock()

	for _, c := range clients {
		_ = c.SetWriteDeadline(time.Now().Add(800 * time.Millisecond))
		if err := writeSimulMsg(c, typ, payload); err != nil {
			simul.mu.Lock()
			if simul.clients != nil {
				delete(simul.clients, c)
			}
			simul.mu.Unlock()
			_ = c.Close()
		}
	}
}

func simulBroadcastState(st simulState) {
	simulBroadcastToClients("state", st)
}

func simulBroadcastSync(ev simulSyncEvent) {
	simulBroadcastToClients("sync", ev)
}

func simulHandleClientConn(ctx context.Context, c net.Conn) {
	defer func() {
		simul.mu.Lock()
		if simul.clients != nil {
			delete(simul.clients, c)
		}

		joiners := 0
		if simul.mode == simulModeHost && simul.clients != nil {
			joiners = len(simul.clients)
		}
		simulSetParticipantsLocked(1 + joiners)
		pc := simul.participants
		simul.mu.Unlock()
		simulRefreshTray()
		if pc > 0 {
			simulBroadcastToClients("participants", simulParticipants{Participants: pc})
		}
		_ = c.Close()
	}()

	dec := json.NewDecoder(c)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	m, err := readSimulMsg(dec)
	if err != nil || m.Type != "hello" {
		return
	}
	var hello simulHello
	_ = json.Unmarshal(m.Data, &hello)
	if !validateJoinCode(hello.Code) {
		_ = writeSimulMsg(c, "error", map[string]any{"error": "invalid code"})
		return
	}

	simul.mu.Lock()
	code := simul.code
	if simul.mode != simulModeHost || code == "" || hello.Code != code {
		simul.mu.Unlock()
		_ = writeSimulMsg(c, "error", map[string]any{"error": "wrong code"})
		return
	}
	if simul.clients == nil {
		simul.clients = make(map[net.Conn]struct{})
	}
	joinersBefore := len(simul.clients)
	joinersAfter := joinersBefore + 1
	invite := simul.inviteText
	pc := 1 + joinersAfter
	st := simul.hostState
	simul.mu.Unlock()

	_ = c.SetReadDeadline(time.Time{})
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := writeSimulMsg(c, "hello_ok", simulHelloOK{Role: "joiner", HostName: "Koushin", HostVer: appVersion, Joiners: joinersAfter, InviteStr: invite}); err != nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = writeSimulMsg(c, "participants", simulParticipants{Participants: pc})
	if strings.TrimSpace(st.Anime) != "" {
		_ = c.SetWriteDeadline(time.Now().Add(800 * time.Millisecond))
		_ = writeSimulMsg(c, "state", st)
	}

	simul.mu.Lock()
	if simul.mode != simulModeHost || simul.code == "" || simul.code != hello.Code {
		simul.mu.Unlock()
		return
	}
	if simul.clients == nil {
		simul.clients = make(map[net.Conn]struct{})
	}
	simul.clients[c] = struct{}{}
	joiners := len(simul.clients)
	simulSetParticipantsLocked(1 + joiners)
	pc2 := simul.participants
	simul.mu.Unlock()
	simulRefreshTray()
	if pc2 > 0 {
		simulBroadcastToClients("participants", simulParticipants{Participants: pc2})
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		msg, err := readSimulMsg(dec)
		if err != nil {
			return
		}
		if msg.Type == "ping" {
			_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_ = writeSimulMsg(c, "pong", nil)
		}
	}
}

func simulStartHost(ctx context.Context) {
	simulLoadSettingsFromStore()

	simul.mu.Lock()
	if simul.mode != simulModeOff {
		simul.mu.Unlock()
		return
	}
	if !validateJoinCode(simul.code) {
		simul.mu.Unlock()
		messageBox("Koushin: Simulwatching", "Invalid host code. Please choose a code (3–18 letters/numbers).", mbOK|mbIconError)
		return
	}
	port := simul.cfg.Port
	autoUPnP := simul.cfg.AutoPortMap
	simul.mode = simulModeHost

	simul.clients = make(map[net.Conn]struct{})
	simulSetParticipantsLocked(1)
	simulSetStatusLocked("Host")
	simul.mu.Unlock()
	simulRefreshTray()

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		simul.mu.Lock()
		simulStopLocked()
		simulSetStatusLocked("Off")
		simul.mu.Unlock()
		simulRefreshTray()
		messageBox("Koushin: Simulwatching", "Could not host Simulwatching:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}

	publicIP0 := ""
	{
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		publicIP0, _ = detectPublicIP(cctx)
		cancel()
	}

	simul.mu.Lock()
	simul.ln = ln
	code := simul.code
	invite := fmt.Sprintf("Invite: %s:%d  Code: %s", firstNonEmpty(publicIP0, "<your-public-ip>"), port, code)
	simul.inviteText = invite
	simulSetStatusLocked("Host")
	simul.mu.Unlock()
	simulRefreshTray()

	messageBox("Koushin: Simulwatching host", invite+"\n\nIf your friend can't connect, you may need to port-forward TCP port "+strconv.Itoa(port)+" to this PC.", mbOK|mbIconInfo)

	go func() {
		defer logRecoveredPanic("simulStartHost.background")
		var cleanup func()
		publicIP := publicIP0
		if autoUPnP {
			cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			cl, ext, perr := tryPortMapUPnP(cctx, port, "Koushin Simulwatching", 3600)
			cancel()
			if perr == nil {
				cleanup = cl
				publicIP = strings.TrimSpace(ext)
			} else {
				cctx2, cancel2 := context.WithTimeout(ctx, 6*time.Second)
				cl2, ext2, perr2 := tryPortMapNATPMP(cctx2, port, 3600)
				cancel2()
				if perr2 == nil {
					cleanup = cl2
					publicIP = strings.TrimSpace(ext2)
				} else {
					logAppend("simul: port mapping failed: ", perr.Error(), " / ", perr2.Error())
				}
			}
		}
		if publicIP == "" {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			publicIP, _ = detectPublicIP(cctx)
			cancel()
		}

		simul.mu.Lock()
		stillHosting := simul.mode == simulModeHost && simul.ln == ln
		if stillHosting {
			if cleanup != nil {
				if simul.portMapOff != nil {
					simul.portMapOff()
				}
				simul.portMapOff = cleanup
			}
			invite2 := fmt.Sprintf("Invite: %s:%d  Code: %s", firstNonEmpty(publicIP, "<your-public-ip>"), port, code)
			simul.inviteText = invite2
			simulSetStatusLocked("Host")
		}
		simul.mu.Unlock()
		if stillHosting {
			simulRefreshTray()
		} else if cleanup != nil {
			cleanup()
		}
	}()

	go func() {
		if !autoUPnP {
			return
		}
		t := time.NewTicker(25 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
				cl, _, perr := tryPortMapUPnP(cctx, port, "Koushin Simulwatching", 3600)
				cancel()
				if perr == nil && cl != nil {
					simul.mu.Lock()
					if simul.portMapOff != nil {
						simul.portMapOff()
					}
					simul.portMapOff = cl
					simul.mu.Unlock()
				}
			}
		}
	}()

	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go simulHandleClientConn(ctx, conn)
		}
	}()
}

func simulApplyJoinStateToUI(cfg Config, mgr *discordManager) {
	simul.mu.Lock()
	if simul.mode != simulModeJoin {
		simul.mu.Unlock()
		return
	}
	st := simul.lastState
	status := simul.status
	simul.mu.Unlock()
	_ = status

	pos := st.Pos
	if !st.Paused && st.Dur > 0 && st.UpdatedAt > 0 {
		delta := float64(time.Now().UnixNano()-st.UpdatedAt) / float64(time.Second)
		pos = pos + delta
		if pos > st.Dur {
			pos = st.Dur
		}
	}

	aname := strings.TrimSpace(st.Anime)
	if aname == "" {
		discordApplyActivity(cfg.DiscordAppID, mgr, nil)
		traySetIdle()
		return
	}

	act := buildActivity(aname, st.episodeLabel(), st.CoverURL, cfg.SmallImage, anilistURL(st.AniID), pos, st.Dur, st.Paused, st.Rewatching)
	if !st.Ready {
		delete(act, "timestamps")
	}
	discordApplyActivity(cfg.DiscordAppID, mgr, act)

	percent := -1
	if st.Dur > 0 {
		p := int(pos/st.Dur*100 + 0.5)
		if p < 0 {
			p = 0
		} else if p > 100 {
			p = 100
		}
		percent = p
	}
	traySetWatching(aname, st.Episode, percent, st.TotalEps)
}

func simulStartJoinUI(ctx context.Context, ua string, onJoin func(addr, code string)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	baseURL := "http://" + addr + "/"

	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Koushin · Simulwatching</title>
<style>
body{font-family:system-ui;max-width:720px;margin:24px auto;line-height:1.4}
input{width:100%;padding:10px;font-size:16px;margin:6px 0}
button{padding:10px 12px;font-size:16px}
small{color:#666}
</style></head>
<body>
<h2>Join Simulwatching</h2>
<p>Enter your friend’s <b>public IP:port</b> and the <b>code</b> they see in Koushin.</p>
<label>Host (IP:PORT)</label>
<input id="host" placeholder="123.45.67.89:45130" />
<label>Code</label>
<input id="code" placeholder="3–18 letters/numbers" maxlength="18" />
<div style="margin-top:10px"><button onclick="join()">Join</button></div>
<p id="status"><small></small></p>
<script>
const statusEl = document.querySelector('#status small');
async function join(){
  const host = document.getElementById('host').value.trim();
  const code = document.getElementById('code').value.trim();
  statusEl.textContent = 'Connecting…';
  const res = await fetch('/api/join', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({host, code})});
  const data = await res.json();
  if (!res.ok) { statusEl.textContent = data.error || 'Failed'; return; }
  statusEl.textContent = 'Joined. You can close this tab.';
  try { window.open('', '_self'); window.close(); } catch (e) {}
  setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 250);
  window.location.replace('/done');
}
</script>
</body></html>`)
	})

	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>Koushin · Done</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Joined</h2>
<p>Attempting to close this tab…</p>
<script>setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 100);</script>
<p><small>If it doesn't close automatically, you can close it now.</small></p>
</body></html>`)
	})

	selected := make(chan struct{}, 1)
	mux.HandleFunc("/api/join", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			Host string `json:"host"`
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid body"})
			return
		}
		body.Host = strings.TrimSpace(body.Host)
		body.Code = strings.TrimSpace(body.Code)
		if body.Host == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "missing host"})
			return
		}
		if !validateJoinCode(body.Code) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid code"})
			return
		}
		if onJoin != nil {
			onJoin(body.Host, body.Code)
		}
		select {
		case selected <- struct{}{}:
		default:
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	go func() { _ = srv.Serve(ln) }()
	go func() {
		select {
		case <-selected:
			time.Sleep(5 * time.Second)
		case <-time.After(5 * time.Minute):
		}
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(c)
		cancel()
	}()

	openBrowser(baseURL)
	return nil
}

func simulStartHostUI(ctx context.Context, onHost func(code string)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	baseURL := "http://" + addr + "/"

	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Koushin · Simulwatching</title>
<style>
body{font-family:system-ui;max-width:720px;margin:24px auto;line-height:1.4}
input{width:100%;padding:10px;font-size:16px;margin:6px 0}
button{padding:10px 12px;font-size:16px}
small{color:#666}
</style></head>
<body>
<h2>Host Simulwatching</h2>
<p>Choose a <b>code</b> for this session. You will enter it each time you host (Koushin does not save it).</p>
<label>Code</label>
<input id="code" placeholder="3–18 letters/numbers" maxlength="18" />
<div style="margin-top:10px"><button onclick="startHosting()">Start hosting</button></div>
<p id="status"><small></small></p>
<script>
const statusEl = document.querySelector('#status small');
async function startHosting(){
  const code = document.getElementById('code').value.trim();
  statusEl.textContent = 'Starting…';
  const res = await fetch('/api/host', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({code})});
  const data = await res.json();
  if (!res.ok) { statusEl.textContent = data.error || 'Failed'; return; }
  statusEl.textContent = 'Hosting started. You can close this tab.';
  try { window.open('', '_self'); window.close(); } catch (e) {}
  setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 250);
  window.location.replace('/done');
}
</script>
</body></html>`)
	})

	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><title>Koushin · Done</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Hosting</h2>
<p>Attempting to close this tab…</p>
<script>setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 100);</script>
<p><small>If it doesn't close automatically, you can close it now.</small></p>
</body></html>`)
	})

	selected := make(chan struct{}, 1)
	mux.HandleFunc("/api/host", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid body"})
			return
		}
		body.Code = strings.TrimSpace(body.Code)
		if !validateJoinCode(body.Code) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid code"})
			return
		}
		if onHost != nil {
			onHost(body.Code)
		}
		select {
		case selected <- struct{}{}:
		default:
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	go func() { _ = srv.Serve(ln) }()
	go func() {
		select {
		case <-selected:
			time.Sleep(5 * time.Second)
		case <-time.After(5 * time.Minute):
		}
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(c)
		cancel()
	}()

	openBrowser(baseURL)
	return nil
}

func simulStartJoin(ctx context.Context, host string, code string) {
	host = strings.TrimSpace(host)
	code = strings.TrimSpace(code)
	if host == "" || !validateJoinCode(code) {
		messageBox("Koushin: Simulwatching", "Invalid host or code.", mbOK|mbIconError)
		return
	}
	simulStop()
	simulLoadSettingsFromStore()

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		messageBox("Koushin: Simulwatching", "Could not connect:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := writeSimulMsg(conn, "hello", simulHello{Code: code}); err != nil {
		_ = conn.Close()
		messageBox("Koushin: Simulwatching", "Handshake failed:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}

	dec := json.NewDecoder(conn)
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	msg, err := readSimulMsg(dec)
	if err != nil {
		_ = conn.Close()
		messageBox("Koushin: Simulwatching", "Handshake failed:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}
	if msg.Type == "error" {
		_ = conn.Close()
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(msg.Data, &e)
		messageBox("Koushin: Simulwatching", "Join rejected:\n\n"+strings.TrimSpace(e.Error), mbOK|mbIconError)
		return
	}
	if msg.Type != "hello_ok" {
		_ = conn.Close()
		messageBox("Koushin: Simulwatching", "Unexpected response from host.", mbOK|mbIconError)
		return
	}

	simul.mu.Lock()
	simul.mode = simulModeJoin
	simul.conn = conn
	simulSetParticipantsLocked(1)
	simulSetStatusLocked("Joined")
	simul.mu.Unlock()
	simulRefreshTray()

	clearTrackingState()

	go func() {
		defer func() {
			_ = conn.Close()
			simul.mu.Lock()
			if simul.conn == conn {
				simul.conn = nil
				simul.mode = simulModeOff
				simulSetParticipantsLocked(0)
				simulSetStatusLocked("Off")
			}
			simul.mu.Unlock()
			simulRefreshTray()
		}()

		go func() {
			t := time.NewTicker(25 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
					if err := writeSimulMsg(conn, "ping", nil); err != nil {
						return
					}
				}
			}
		}()

		synced := make(map[string]bool)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
			m, err := readSimulMsg(dec)
			if err != nil {
				return
			}
			switch m.Type {
			case "state":
				var st simulState
				if json.Unmarshal(m.Data, &st) == nil {
					st.UpdatedAt = time.Now().UnixNano()
					simul.mu.Lock()
					simul.lastState = st
					simul.mu.Unlock()
				}
			case "sync":
				var ev simulSyncEvent
				if json.Unmarshal(m.Data, &ev) == nil {
					k := fmt.Sprintf("%d:%d", ev.AniID, ev.Episode)
					if synced[k] {
						continue
					}
					synced[k] = true
					store.mu.RLock()
					joinSync := store.SimulJoinSync
					tok := store.AccessToken
					uid := store.UserID
					store.mu.RUnlock()
					if joinSync && strings.TrimSpace(tok) != "" && uid > 0 && ev.AniID > 0 && ev.Episode > 0 {
						c2, cancel := context.WithTimeout(context.Background(), 8*time.Second)
						_ = syncAniListProgress(c2, tok, uid, ev.AniID, ev.Episode, ev.TotalEps)
						cancel()
					}
				}
			case "participants":
				var p simulParticipants
				if json.Unmarshal(m.Data, &p) == nil {
					simul.mu.Lock()
					simulSetParticipantsLocked(p.Participants)
					simul.mu.Unlock()
					simulRefreshTray()
				}
			case "pong":
			default:
			}
		}
	}()
}

func writeSimulMsg(w io.Writer, typ string, payload any) error {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = b
	}
	b, err := json.Marshal(simulMessage{Type: typ, TS: time.Now().Unix(), Data: raw})
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func readSimulMsg(dec *json.Decoder) (simulMessage, error) {
	var m simulMessage
	err := dec.Decode(&m)
	return m, err
}

func detectPublicIP(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org?format=text", nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("ipify http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	ip := strings.TrimSpace(string(b))
	if net.ParseIP(ip) == nil {
		return "", errors.New("invalid public IP")
	}
	return ip, nil
}

func tryPortMapUPnP(ctx context.Context, port int, desc string, lifetimeSeconds uint32) (func(), string, error) {
	if port <= 0 || port > 65535 {
		return nil, "", errors.New("invalid port")
	}
	try := func() (func(), string, error) {
		clients, _, err := internetgateway2.NewWANIPConnection2Clients()
		if err == nil && len(clients) > 0 {
			c := clients[0]
			extIP, _ := c.GetExternalIPAddress()
			_ = c.DeletePortMapping("", uint16(port), "TCP")
			if err := c.AddPortMapping("", uint16(port), "TCP", uint16(port), "", true, desc, lifetimeSeconds); err != nil {
				return nil, "", err
			}
			cleanup := func() { _ = c.DeletePortMapping("", uint16(port), "TCP") }
			return cleanup, extIP, nil
		}
		clients1, _, err1 := internetgateway1.NewWANIPConnection1Clients()
		if err1 == nil && len(clients1) > 0 {
			c := clients1[0]
			extIP, _ := c.GetExternalIPAddress()
			_ = c.DeletePortMapping("", uint16(port), "TCP")
			if err := c.AddPortMapping("", uint16(port), "TCP", uint16(port), "", true, desc, lifetimeSeconds); err != nil {
				return nil, "", err
			}
			cleanup := func() { _ = c.DeletePortMapping("", uint16(port), "TCP") }
			return cleanup, extIP, nil
		}
		return nil, "", errors.New("no UPnP IGD found")
	}

	done := make(chan struct{})
	var cleanup func()
	var extIP string
	var outErr error
	go func() {
		defer close(done)
		cleanup, extIP, outErr = try()
	}()
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case <-done:
		return cleanup, extIP, outErr
	}
}

func tryPortMapNATPMP(ctx context.Context, port int, lifetimeSeconds int) (func(), string, error) {
	gateways := []string{"192.168.0.1", "192.168.1.1"}
	for _, gw := range gateways {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		default:
		}

		done := make(chan struct{})
		var (
			cleanup func()
			err     error
		)
		go func(gw string) {
			defer close(done)
			c := natpmp.NewClient(net.ParseIP(gw))
			if c == nil {
				err = errors.New("invalid gateway")
				return
			}
			_, _ = c.GetExternalAddress()
			res, e := c.AddPortMapping("tcp", port, port, lifetimeSeconds)
			if e != nil {
				err = e
				return
			}
			_ = res
			cleanup = func() { _, _ = c.AddPortMapping("tcp", port, port, 0) }
		}(gw)

		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(2 * time.Second):
			continue
		case <-done:
			if err == nil && cleanup != nil {
				return cleanup, "", nil
			}
		}
	}
	return nil, "", errors.New("no NAT-PMP gateway responded")
}

func defaultSimulPort() int { return 45130 }

func validateSimulPort(p int) bool {
	return p >= 1024 && p <= 65535
}

func validateJoinCode(code string) bool {
	code = strings.TrimSpace(code)
	if len(code) < 3 || len(code) > 18 {
		return false
	}
	for _, r := range code {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func (s simulState) episodeLabel() string {
	ep := strings.TrimSpace(s.Episode)
	if ep == "" {
		return ""
	}
	if s.TotalEps > 0 {
		return fmt.Sprintf("%s/%d", ep, s.TotalEps)
	}
	return fmt.Sprintf("%s/??", ep)
}

var (
	menuSimul         *systray.MenuItem
	menuSimulHost     *systray.MenuItem
	menuSimulJoin     *systray.MenuItem
	menuSimulStop     *systray.MenuItem
	menuSimulStatus   *systray.MenuItem
	menuSimulJoinSync *systray.MenuItem
)

func simulJoined() bool {
	simul.mu.Lock()
	defer simul.mu.Unlock()
	return simul.mode == simulModeJoin
}

func simulHosting() bool {
	simul.mu.Lock()
	defer simul.mu.Unlock()
	return simul.mode == simulModeHost
}

// simulPublishIdle tells joiners that the host stopped playing.
func simulPublishIdle() {
	if !simulHosting() {
		return
	}
	st := simulState{Ready: false, UpdatedAt: time.Now().UnixNano()}
	simul.mu.Lock()
	simul.hostState = st
	simul.mu.Unlock()
	simulBroadcastState(st)
}

// simulPublish sends the host's playback state to every joiner.
func simulPublish(p playbackSnapshot) {
	if !simulHosting() {
		return
	}
	st := simulState{
		Anime:      p.Anime,
		Episode:    p.Episode,
		AniID:      p.AniID,
		TotalEps:   p.TotalEps,
		CoverURL:   p.CoverURL,
		Rewatching: p.Rewatching,
		Paused:     p.Paused,
		Pos:        p.Pos,
		Dur:        p.Dur,
		Ready:      p.Ready,
		UpdatedAt:  time.Now().UnixNano(),
		HostFile:   p.HostFile,
		HostSeason: p.HostSeason,
	}
	simul.mu.Lock()
	simul.hostState = st
	simul.mu.Unlock()
	simulBroadcastState(st)
}

// simulPublishSync forwards an 80%-watched event so joiners can sync too.
func simulPublishSync(aniID, episode, totalEps int) {
	simulBroadcastSync(simulSyncEvent{AniID: aniID, Episode: episode, TotalEps: totalEps, AtUnixSec: time.Now().Unix()})
}

// addSimulMenu adds the Simulwatching submenu and handles its clicks.
func addSimulMenu(ctx context.Context) {
	menuSimul = systray.AddMenuItem("Simulwatching", "Host or join a Simulwatching session")
	menuSimulHost = menuSimul.AddSubMenuItem("Host a Simul…", "Host a session and share your watch state")
	menuSimulJoin = menuSimul.AddSubMenuItem("Join a Simul…", "Join a friend's hosted session")
	menuSimulStop = menuSimul.AddSubMenuItem("Stop Simulwatching", "Stop hosting/joining")
	menuSimulStatus = menuSimul.AddSubMenuItem("Status: Off", "Shows current Simulwatching state")
	menuSimulStatus.Disable()
	store.mu.RLock()
	joinSync := store.SimulJoinSync
	store.mu.RUnlock()
	menuSimulJoinSync = menuSimul.AddSubMenuItemCheckbox("Sync my AniList",
		"When joined, sync your own AniList based on the host's 80% progress events", joinSync)
	simulLoadSettingsFromStore()
	simulRefreshTray()
	systray.AddSeparator()

	go func() {
		for {
			select {
			case <-menuSimulJoinSync.ClickedCh:
				setChecked(menuSimulJoinSync, store.toggle(&store.SimulJoinSync))

			case <-menuSimulHost.ClickedCh:
				go func() {
					_ = simulStartHostUI(ctx, func(code string) {
						simul.mu.Lock()
						simul.code = code
						simul.mu.Unlock()
						go simulStartHost(ctx)
					})
				}()

			case <-menuSimulJoin.ClickedCh:
				go func() {
					_ = simulStartJoinUI(ctx, loadConfig().UserAgent, func(addr, code string) {
						go simulStartJoin(ctx, addr, code)
					})
				}()

			case <-menuSimulStop.ClickedCh:
				simulStop()

			case <-ctx.Done():
				return
			}
		}
	}()
}
