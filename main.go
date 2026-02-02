package main

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	habari "github.com/5rahim/habari"
	"github.com/getlantern/systray"
	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/dcps/internetgateway2"
	natpmp "github.com/jackpal/go-nat-pmp"
	"github.com/natefinch/npipe"
	"golang.org/x/sys/windows/registry"
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
		messageBox("Koushin — Simulwatching", "Invalid host code. Please choose a code (3–18 letters/numbers).", mbOK|mbIconError)
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
		messageBox("Koushin — Simulwatching", "Could not host Simulwatching:\n\n"+err.Error(), mbOK|mbIconError)
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

	messageBox("Koushin — Simulwatching host", invite+"\n\nIf your friend can't connect, you may need to port-forward TCP port "+strconv.Itoa(port)+" to this PC.", mbOK|mbIconInfo)

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

	act := buildActivity(aname, st.episodeLabel(), "", st.CoverURL, cfg.SmallImage, anilistURL(st.AniID), pos, st.Dur, st.Paused)
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
		messageBox("Koushin — Simulwatching", "Invalid host or code.", mbOK|mbIconError)
		return
	}
	simulStop()
	simulLoadSettingsFromStore()

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		messageBox("Koushin — Simulwatching", "Could not connect:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := writeSimulMsg(conn, "hello", simulHello{Code: code}); err != nil {
		_ = conn.Close()
		messageBox("Koushin — Simulwatching", "Handshake failed:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}

	dec := json.NewDecoder(conn)
	_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	msg, err := readSimulMsg(dec)
	if err != nil {
		_ = conn.Close()
		messageBox("Koushin — Simulwatching", "Handshake failed:\n\n"+err.Error(), mbOK|mbIconError)
		return
	}
	if msg.Type == "error" {
		_ = conn.Close()
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(msg.Data, &e)
		messageBox("Koushin — Simulwatching", "Join rejected:\n\n"+strings.TrimSpace(e.Error), mbOK|mbIconError)
		return
	}
	if msg.Type != "hello_ok" {
		_ = conn.Close()
		messageBox("Koushin — Simulwatching", "Unexpected response from host.", mbOK|mbIconError)
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

func discordRPCEnabled() bool {
	store.mu.RLock()
	on := store.DiscordRPC
	store.mu.RUnlock()
	return on
}

func discordApplyActivity(appID string, mgr *discordManager, activity map[string]any) {
	if mgr == nil {
		return
	}
	if !discordRPCEnabled() {
		mgr.close(appID)
		return
	}
	mgr.setActivity(appID, activity)
}

func randJoinCode() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%06d", mrand.Intn(1000000))
	}
	const hextbl = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 0; i < len(b); i++ {
		out[i*2] = hextbl[b[i]>>4]
		out[i*2+1] = hextbl[b[i]&0x0f]
	}
	return string(out)
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

const mpvRPCDeadline = 800 * time.Millisecond

//go:embed koushin.ico
var iconICO []byte

const fallbackLargeImageKey = "koushin"

func setTrayIcon() {
	if len(iconICO) > 0 {
		systray.SetIcon(iconICO)
		return
	}
	if b, err := os.ReadFile("koushin.ico"); err == nil {
		systray.SetIcon(b)
	}
}

func logPath() string {
	return filepath.Join(appDataDir(), "koushin.log")
}

func logAppend(lines ...string) {
	f, err := os.OpenFile(logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	for _, s := range lines {
		ts := time.Now().Format("2006-01-02 15:04:05")
		_, _ = f.WriteString(ts + " " + s + "\n")
	}
}

func logRecoveredPanic(where string) {
	if r := recover(); r != nil {
		logAppend("panic in "+where+": ", fmt.Sprint(r))
		logAppend(string(debug.Stack()))
	}
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

const windowsRunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
const windowsRunValueName = "Koushin"

func setWindowsRunOnStartup(enabled bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, windowsRunKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	if !enabled {
		err := k.DeleteValue(windowsRunValueName)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	cmd := "\"" + exe + "\""
	return k.SetStringValue(windowsRunValueName, cmd)
}

func startMenuShortcutPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = strings.TrimSpace(os.Getenv("APPDATA"))
	}
	if base == "" {
		return "", errors.New("could not determine AppData directory")
	}
	return filepath.Join(base, "Microsoft", "Windows", "Start Menu", "Programs", "Koushin", "Koushin.lnk"), nil
}

func escapePowerShellSingleQuoted(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func ensureStartMenuShortcut() error {
	lnkPath, err := startMenuShortcutPath()
	if err != nil {
		return err
	}
	if fileExists(lnkPath) {
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	wd := filepath.Dir(exe)

	if err := os.MkdirAll(filepath.Dir(lnkPath), 0700); err != nil {
		return err
	}

	ps := "$WshShell = New-Object -ComObject WScript.Shell; " +
		"$Shortcut = $WshShell.CreateShortcut('" + escapePowerShellSingleQuoted(lnkPath) + "'); " +
		"$Shortcut.TargetPath = '" + escapePowerShellSingleQuoted(exe) + "'; " +
		"$Shortcut.WorkingDirectory = '" + escapePowerShellSingleQuoted(wd) + "'; " +
		"$Shortcut.IconLocation = '" + escapePowerShellSingleQuoted(exe) + ",0'; " +
		"$Shortcut.Description = 'Koushin'; " +
		"$Shortcut.Save();"

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", ps)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create start menu shortcut: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

const (
	anilistClientID = "31833"
	discordAppID    = "1434412611411120198"

	localLoginAddr = "127.0.0.1:45124"
	anilistGQLURL  = "https://graphql.anilist.co"
)

var errAniListRateLimited = errors.New("anilist rate limited")

type Config struct {
	DiscordAppID string
	MpvPipe      string
	PollInterval time.Duration
	UserAgent    string
	SmallImage   string
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

func simulModeTitle(m simulMode) string {
	switch m {
	case simulModeHost:
		return "Host"
	case simulModeJoin:
		return "Joined"
	default:
		return "Off"
	}
}

type resolveRequest struct {
	SeriesKey string
	MediaID   int
}

var resolveCh = make(chan resolveRequest, 8)

func loadConfig() Config {
	appID := discordAppID
	pipe := os.Getenv("MPV_PIPE")
	if pipe == "" {
		pipe = `\\.\pipe\mpv-pipe`
	}
	poll := 250 * time.Millisecond
	if v := os.Getenv("POLL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 200 {
			poll = time.Duration(ms) * time.Millisecond
		}
	}
	ua := os.Getenv("HTTP_USER_AGENT")
	if ua == "" {
		ua = "koushin/1.2 (+https://anilist.co)"
	}
	small := "anilist"
	return Config{
		DiscordAppID: appID,
		MpvPipe:      pipe,
		PollInterval: poll,
		UserAgent:    ua,
		SmallImage:   small,
	}
}

const appVersion = "0.2.2"

const (
	githubOwner = "hyuzipt"
	githubRepo  = "Koushin"
)

type mpvRequest struct {
	Command   []any `json:"command"`
	RequestID int64 `json:"request_id,omitempty"`
}

type mpvResponse struct {
	Error     string      `json:"error"`
	Data      interface{} `json:"data,omitempty"`
	RequestID int64       `json:"request_id,omitempty"`
}

var mpvNextRequestID int64

func mpvAlive(conn net.Conn) bool {
	_, err := mpvSend(conn, "get_property", "mpv-version")
	return err == nil
}

func mpvSend(conn net.Conn, cmd ...any) (interface{}, error) {
	rid := atomic.AddInt64(&mpvNextRequestID, 1)
	req := mpvRequest{Command: cmd, RequestID: rid}
	b, _ := json.Marshal(req)
	b = append(b, '\n')

	_ = conn.SetWriteDeadline(time.Now().Add(mpvRPCDeadline))
	if _, err := conn.Write(b); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(mpvRPCDeadline))
	dec := json.NewDecoder(conn)
	for {
		var resp mpvResponse
		if err := dec.Decode(&resp); err != nil {
			return nil, err
		}
		if resp.RequestID != 0 && resp.RequestID != rid {
			continue
		}
		if resp.Error != "success" {
			return nil, fmt.Errorf("mpv error: %s", resp.Error)
		}
		return resp.Data, nil
	}
}

type mpvState struct {
	Path       string
	FileName   string
	MediaTitle string
	Duration   float64
	TimePos    float64
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

	if d, err := mpvSend(conn, "get_property", "path"); err == nil {
		if s, ok := d.(string); ok {
			st.Path = s
		}
	}
	if d, err := mpvSend(conn, "get_property", "filename"); err == nil {
		if s, ok := d.(string); ok {
			st.FileName = s
		}
	}
	if d, err := mpvSend(conn, "get_property", "media-title"); err == nil {
		if s, ok := d.(string); ok {
			st.MediaTitle = s
		}
	}
	if d, err := mpvSend(conn, "get_property", "duration"); err == nil {
		if f, ok := asFloat(d); ok && f > 0 {
			st.Duration = f
		}
	}
	if d, err := mpvSend(conn, "get_property", "time-pos"); err == nil {
		if f, ok := asFloat(d); ok && f >= 0 {
			st.TimePos = f
		}
	}
	if d, err := mpvSend(conn, "get_property", "pause"); err == nil {
		if b, ok := d.(bool); ok {
			st.Pause = b
		}
	}
	if st.FileName == "" && st.MediaTitle == "" {
		return st, errors.New("no file playing")
	}
	return st, nil
}

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
	Episodes int    `json:"episodes"`
	Format   string `json:"format"`
}

type anilistResp struct {
	Data struct {
		Page struct {
			Media []mediaLite `json:"media"`
		} `json:"Page"`
	} `json:"data"`
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

const (
	aniStatusCurrent   = "CURRENT"
	aniStatusPlanning  = "PLANNING"
	aniStatusCompleted = "COMPLETED"
	aniStatusDropped   = "DROPPED"
	aniStatusPaused    = "PAUSED"
	aniStatusRepeating = "REPEATING"
)

var (
	reParensYear = regexp.MustCompile(`\s*\((19|20)\d{2}\)`)
	reEpTail     = regexp.MustCompile(`\s*[-–—]\s*(?:ep|episode)?\s*\d{1,4}\s*$`)
	reBrackets   = regexp.MustCompile(`\s*[\[\(][^\]\)]*[\]\)]`)
	reMultiSpace = regexp.MustCompile(`\s{2,}`)
	reAnyYear    = regexp.MustCompile(`\b(19|20)\d{2}\b`)

	reSxxEyy      = regexp.MustCompile(`(?i)\bS(\d{1,2})[ ._-]*E(\d{1,3})\b`)
	reSxxEyyLoose = regexp.MustCompile(`(?i)\bS\d{1,2}[ ._-]*E\d{1,3}\b`)

	reSeasonSNum = regexp.MustCompile(`(?i)\bS(\d{1,2})\b`)
	reSeasonWord = regexp.MustCompile(`(?i)\b(?:season|cour)\s*(\d{1,2})\b`)
	reSeasonOrd  = regexp.MustCompile(`(?i)\b(1st|2nd|3rd|[4-9]th)\s+season\b`)

	reTrailingCRC = regexp.MustCompile(`\s*\[[0-9a-fA-F]{6,}\]\s*$`)
)

var zeroBasedRelease = struct {
	mu    sync.Mutex
	bySig map[string]bool
}{bySig: make(map[string]bool)}

func zeroBasedStorePath() string { return filepath.Join(appDataDir(), "zero_based_releases.json") }

func loadZeroBasedSignatures() {
	b, err := os.ReadFile(zeroBasedStorePath())
	if err != nil {
		return
	}
	var tmp struct {
		Sigs []string `json:"sigs"`
	}
	if json.Unmarshal(b, &tmp) != nil {
		return
	}
	zeroBasedRelease.mu.Lock()
	for _, s := range tmp.Sigs {
		s = strings.TrimSpace(strings.ToLower(s))
		if s != "" {
			zeroBasedRelease.bySig[s] = true
		}
	}
	zeroBasedRelease.mu.Unlock()
}

func saveZeroBasedSignaturesLocked() {

	sigs := make([]string, 0, len(zeroBasedRelease.bySig))
	for s := range zeroBasedRelease.bySig {
		sigs = append(sigs, s)
	}
	sort.Strings(sigs)
	b, _ := json.MarshalIndent(struct {
		Sigs []string `json:"sigs"`
	}{Sigs: sigs}, "", "  ")
	_ = os.WriteFile(zeroBasedStorePath(), b, 0600)
}

func releaseSignatureForZeroBased(nameOrPath string) (sig string, hasEpisodeToken bool) {
	base := filepath.Base(strings.TrimSpace(nameOrPath))
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return "", false
	}

	base = reTrailingCRC.ReplaceAllString(base, "")
	base = strings.TrimSpace(base)
	if base == "" {
		return "", false
	}

	validDigitsAllowZero := func(digits string, allow4Digits bool) bool {
		digits = strings.TrimSpace(digits)
		if digits == "" {
			return false
		}

		if strings.TrimLeft(digits, "0") == "" {
			return true
		}
		return normalizeEpisodeCandidate(digits, allow4Digits) != ""
	}

	hasEpisodeToken = reSxxEyy.MatchString(base)
	if hasEpisodeToken {
		base = reSxxEyy.ReplaceAllStringFunc(base, func(m string) string {
			sm := reSxxEyy.FindStringSubmatch(m)
			if len(sm) == 3 {
				return "S" + sm[1] + "E__"
			}
			return "S__E__"
		})

		base = reMultiSpace.ReplaceAllString(base, " ")
		base = strings.TrimSpace(base)
		if base == "" {
			return "", false
		}
		return strings.ToLower(base), true
	}

	if idx := reEyyOnly.FindStringSubmatchIndex(base); len(idx) == 4 {
		digits := base[idx[2]:idx[3]]
		if validDigitsAllowZero(digits, false) {
			base = base[:idx[2]] + "__" + base[idx[3]:]
			base = reMultiSpace.ReplaceAllString(base, " ")
			base = strings.TrimSpace(base)
			if base == "" {
				return "", false
			}
			return strings.ToLower(base), true
		}
	}

	if ms := reEGeneric.FindAllStringSubmatchIndex(base, -1); len(ms) > 0 {
		for _, idx := range ms {
			if len(idx) != 4 {
				continue
			}
			digits := base[idx[2]:idx[3]]
			if !validDigitsAllowZero(digits, true) {
				continue
			}
			base2 := base[:idx[2]] + "__" + base[idx[3]:]
			base2 = reMultiSpace.ReplaceAllString(base2, " ")
			base2 = strings.TrimSpace(base2)
			if base2 == "" {
				continue
			}
			return strings.ToLower(base2), true
		}
	}

	if ms := reDashEp.FindAllStringSubmatchIndex(base, -1); len(ms) > 0 {
		for _, idx := range ms {
			if len(idx) != 4 {
				continue
			}
			digits := base[idx[2]:idx[3]]
			if !validDigitsAllowZero(digits, false) {
				continue
			}
			base2 := base[:idx[2]] + "__" + base[idx[3]:]
			base2 = reMultiSpace.ReplaceAllString(base2, " ")
			base2 = strings.TrimSpace(base2)
			if base2 == "" {
				continue
			}
			return strings.ToLower(base2), true
		}
	}

	if idx := reTrailingNum.FindStringSubmatchIndex(base); len(idx) == 4 {
		digits := base[idx[2]:idx[3]]
		if !validDigitsAllowZero(digits, false) {
			return "", false
		}

		base = base[:idx[2]] + "__" + base[idx[3]:]
		base = reMultiSpace.ReplaceAllString(base, " ")
		base = strings.TrimSpace(base)
		if base == "" {
			return "", false
		}
		return strings.ToLower(base), true
	}

	return "", false
}

func markZeroBasedSignature(sig string) {
	if strings.TrimSpace(sig) == "" {
		return
	}
	zeroBasedRelease.mu.Lock()
	if !zeroBasedRelease.bySig[sig] {
		zeroBasedRelease.bySig[sig] = true
		saveZeroBasedSignaturesLocked()
	}
	zeroBasedRelease.mu.Unlock()
}

func isZeroBasedSignature(sig string) bool {
	zeroBasedRelease.mu.Lock()
	on := zeroBasedRelease.bySig[sig]
	zeroBasedRelease.mu.Unlock()
	return on
}

func dirContainsEpisodeZeroForSignature(filePath string, sig string) bool {
	if strings.TrimSpace(filePath) == "" || strings.TrimSpace(sig) == "" {
		return false
	}
	dir := filepath.Dir(filePath)
	if dir == "" || dir == "." {
		return false
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	limit := 600
	if len(ents) < limit {
		limit = len(ents)
	}
	for i := 0; i < limit; i++ {
		e := ents[i]
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lsig, ok := releaseSignatureForZeroBased(name)
		if !ok || lsig != sig {
			continue
		}

		base := filepath.Base(strings.TrimSpace(name))
		if j := strings.LastIndexByte(base, '.'); j >= 0 {
			base = base[:j]
		}
		base = strings.TrimSpace(reTrailingCRC.ReplaceAllString(base, ""))
		if base == "" {
			continue
		}
		if m := reSxxEyy.FindStringSubmatch(base); len(m) == 3 {
			d := strings.TrimSpace(m[2])
			if strings.TrimLeft(d, "0") == "" {
				return true
			}
			continue
		}
		if m := reEyyOnly.FindStringSubmatch(base); len(m) == 2 {
			d := strings.TrimSpace(m[1])
			if strings.TrimLeft(d, "0") == "" {
				return true
			}
		}
		if ms := reEGeneric.FindAllStringSubmatch(base, -1); len(ms) > 0 {
			for _, m := range ms {
				if len(m) == 2 {
					d := strings.TrimSpace(m[1])
					if strings.TrimLeft(d, "0") == "" {
						return true
					}
				}
			}
		}
		if ms := reDashEp.FindAllStringSubmatch(base, -1); len(ms) > 0 {
			for _, m := range ms {
				if len(m) == 2 {
					d := strings.TrimSpace(m[1])
					if strings.TrimLeft(d, "0") == "" {
						return true
					}
				}
			}
		}
		if m := reTrailingNum.FindStringSubmatch(base); len(m) == 2 {
			d := strings.TrimSpace(m[1])
			if strings.TrimLeft(d, "0") == "" {
				return true
			}
		}
	}
	return false
}

func applyZeroBasedEpisodeFix(filePath string, key string, ep string) string {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return ep
	}
	n, err := strconv.Atoi(ep)
	if err != nil {
		return ep
	}
	sig, ok := releaseSignatureForZeroBased(key)
	if !ok || sig == "" {
		sig, ok = releaseSignatureForZeroBased(filePath)
	}
	if n == 0 {
		if ok && sig != "" {
			markZeroBasedSignature(sig)
		} else {
			if fsig, fok := releaseSignatureForZeroBased(filePath); fok && fsig != "" {
				markZeroBasedSignature(fsig)
			}
		}
		return "1"
	}
	if !ok || sig == "" {
		return ep
	}
	if isZeroBasedSignature(sig) || dirContainsEpisodeZeroForSignature(filePath, sig) {
		markZeroBasedSignature(sig)
		return strconv.Itoa(n + 1)
	}
	return ep
}

func parseSeasonFromString(s string) int {
	if s == "" {
		return 0
	}
	if m := reSxxEyy.FindStringSubmatch(s); len(m) == 3 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}

	if m := reSeasonSNum.FindStringSubmatch(s); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	if m := reSeasonWord.FindStringSubmatch(s); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	if m := reSeasonOrd.FindStringSubmatch(s); len(m) == 2 {
		s := strings.ToLower(m[1])
		switch s {
		case "1st":
			return 1
		case "2nd":
			return 2
		case "3rd":
			return 3
		default:
			digits := s[:len(s)-2]
			if n, err := strconv.Atoi(digits); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

func wantSeasonFrom(sources ...string) int {
	for _, s := range sources {
		if n := parseSeasonFromString(s); n > 0 {
			return n
		}
	}
	return 0
}

func parseYearString(s string) int {
	if s == "" {
		return 0
	}
	if m := reAnyYear.FindString(s); m != "" {
		if y, err := strconv.Atoi(m); err == nil {
			return y
		}
	}
	return 0
}

func wantYearFrom(md *habari.Metadata, key, mediaTitle string) int {
	if y, err := strconv.Atoi(strings.TrimSpace(md.Year)); err == nil && y > 1900 {
		return y
	}
	if y := parseYearString(key); y > 0 {
		return y
	}
	if y := parseYearString(mediaTitle); y > 0 {
		return y
	}
	return 0
}

func cleanTitleForSearch(s string) string {
	s = strings.TrimSpace(s)
	s = reParensYear.ReplaceAllString(s, "")
	s = reEpTail.ReplaceAllString(s, "")
	s = reBrackets.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, ".", " ")
	s = reAnyYear.ReplaceAllString(s, "")
	s = reSxxEyyLoose.ReplaceAllString(s, "")
	s = reMultiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func seriesKeyForOverride(fileKey, parsedTitle, mediaTitle string, wantYear, wantSeason int) string {
	base := firstNonEmpty(parsedTitle, mediaTitle, fileKey)
	base = cleanTitleForSearch(base)
	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" {
		return ""
	}
	key := base
	if wantYear > 0 {
		key += fmt.Sprintf("#y%d", wantYear)
	}
	if wantSeason > 0 {
		key += fmt.Sprintf("#s%d", wantSeason)
	}
	return key
}

func pickBest(ms []mediaLite, wantYear, wantSeason int) (n string, c string, i int, totalEps int) {
	if len(ms) == 0 {
		return "", "", 0, 0
	}

	base := ms
	if wantSeason > 0 {
		tv := make([]mediaLite, 0, len(ms))
		for _, m := range ms {
			if strings.EqualFold(m.Format, "TV") {
				tv = append(tv, m)
			}
		}
		if len(tv) > 0 {
			base = tv
		}
	}

	sort.Slice(base, func(i, j int) bool {
		yi, yj := base[i].StartDate.Year, base[j].StartDate.Year
		if yi == 0 && yj != 0 {
			return false
		}
		if yi != 0 && yj == 0 {
			return true
		}
		if yi != yj {
			return yi < yj
		}
		return base[i].ID < base[j].ID
	})

	if wantSeason > 0 {
		if wantSeason <= len(base) {
			m := base[wantSeason-1]
			n = strings.TrimSpace(m.Title.English)
			if n == "" {
				n = firstNonEmpty(m.Title.Romaji, m.Title.Native)
			}
			return n, m.CoverImage.Large, m.ID, m.Episodes
		}
	}

	if wantYear > 0 {
		for _, m := range base {
			if m.StartDate.Year == wantYear {
				n = strings.TrimSpace(m.Title.English)
				if n == "" {
					n = firstNonEmpty(m.Title.Romaji, m.Title.Native)
				}
				return n, m.CoverImage.Large, m.ID, m.Episodes
			}
		}
	}

	m := ms[0]
	n = strings.TrimSpace(m.Title.English)
	if n == "" {
		n = firstNonEmpty(m.Title.Romaji, m.Title.Native)
	}
	return n, m.CoverImage.Large, m.ID, m.Episodes
}

func findAniList(ctx context.Context, rawTitle string, wantYear, wantSeason int, ua string) (name, coverURL string, id int, totalEps int, err error) {
	cands := []string{cleanTitleForSearch(rawTitle), rawTitle}
	const q = `
query($search: String) {
  Page(perPage: 10) {
    media(search: $search, type: ANIME, sort: [SEARCH_MATCH, START_DATE_DESC]) {
      id
      title { romaji english native }
      coverImage { large }
      startDate { year }
      episodes
      format
    }
  }
}`
	for _, title := range cands {
		body := map[string]any{"query": q, "variables": map[string]any{"search": title}}
		bs, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
		req.Header.Set("Content-Type", "application/json")
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		resp, rerr := httpClient.Do(req)
		if rerr != nil {
			err = rerr
			continue
		}
		func() {
			defer resp.Body.Close()

			if resp.StatusCode == 429 {
				_, _ = io.Copy(io.Discard, resp.Body)
				err = errAniListRateLimited
				return
			}
			if resp.StatusCode != 200 {
				b, _ := io.ReadAll(resp.Body)
				err = fmt.Errorf("anilist http %d: %s", resp.StatusCode, string(b))
				return
			}
			var ar anilistResp
			if derr := json.NewDecoder(resp.Body).Decode(&ar); derr != nil {
				err = derr
				return
			}
			ms := ar.Data.Page.Media
			if len(ms) == 0 {
				err = errors.New("no match on AniList")
				return
			}
			name, coverURL, id, totalEps = pickBest(ms, wantYear, wantSeason)
			err = nil
		}()
		if errors.Is(err, errAniListRateLimited) {
			return
		}
		if err == nil && id != 0 {
			return
		}
	}
	if err == nil {
		err = errors.New("no match on AniList")
	}
	return
}

func searchAniList(ctx context.Context, query string, ua string) ([]mediaLite, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty query")
	}
	const q = `
query($search: String) {
  Page(perPage: 25) {
    media(search: $search, type: ANIME, sort: [SEARCH_MATCH, START_DATE_DESC]) {
      id
      title { romaji english native }
      coverImage { large }
      startDate { year }
      episodes
      format
    }
  }
}`
	body := map[string]any{"query": q, "variables": map[string]any{"search": query}}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, errAniListRateLimited
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anilist http %d: %s", resp.StatusCode, string(b))
	}
	var ar anilistResp
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, err
	}
	if len(ar.Data.Page.Media) == 0 {
		return nil, errors.New("no match on AniList")
	}
	return ar.Data.Page.Media, nil
}

func getAniListMediaByID(ctx context.Context, id int, ua string) (mediaLite, error) {
	var out mediaLite
	if id <= 0 {
		return out, errors.New("invalid id")
	}
	const q = `
query($id: Int) {
  Media(id: $id, type: ANIME) {
    id
    title { romaji english native }
    coverImage { large }
    startDate { year }
    episodes
    format
  }
}`
	body := map[string]any{"query": q, "variables": map[string]any{"id": id}}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return out, errAniListRateLimited
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return out, fmt.Errorf("anilist http %d: %s", resp.StatusCode, string(b))
	}
	var respObj struct {
		Data struct {
			Media *mediaLite `json:"Media"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respObj); err != nil {
		return out, err
	}
	if respObj.Data.Media == nil {
		return out, errors.New("media not found")
	}
	return *respObj.Data.Media, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func anilistURL(id int) string {
	if id <= 0 {
		return ""
	}
	return fmt.Sprintf("https://anilist.co/anime/%d", id)
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func normalizeVersion(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	return s
}

func isNewerVersion(current, latest string) bool {
	return normalizeVersion(current) != normalizeVersion(latest)
}

func fetchLatestRelease(ctx context.Context) (tag, exeURL string, err error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)

	req, _ := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	req.Header.Set("User-Agent", "koushin-updater")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("GitHub latest release http %d: %s", resp.StatusCode, string(body))
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", err
	}

	if rel.TagName == "" {
		return "", "", errors.New("no tag_name in GitHub release")
	}

	var exe string
	for _, a := range rel.Assets {
		if strings.EqualFold(a.Name, "Koushin.exe") {
			exe = a.BrowserDownloadURL
			break
		}
	}
	if exe == "" {
		for _, a := range rel.Assets {
			if strings.HasSuffix(strings.ToLower(a.Name), ".exe") {
				exe = a.BrowserDownloadURL
				break
			}
		}
	}

	if exe == "" {
		return "", "", errors.New("no .exe asset found in latest release")
	}
	return rel.TagName, exe, nil
}

func performSelfUpdate(ctx context.Context, newTag, exeURL string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get exe path: %w", err)
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		return fmt.Errorf("abs exe path: %w", err)
	}
	dir := filepath.Dir(exePath)

	newPath := filepath.Join(dir, "Koushin_new.exe")

	req, _ := http.NewRequestWithContext(ctx, "GET", exeURL, nil)
	req.Header.Set("User-Agent", "koushin-updater")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("download http %d: %s", resp.StatusCode, string(body))
	}

	tmpPath := newPath + ".part"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create temp exe: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("write temp exe: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, newPath); err != nil {
		return fmt.Errorf("rename temp->new exe: %w", err)
	}

	batPath := filepath.Join(dir, "koushin_update.bat")

	script := `@echo off
setlocal
set EXE=%~1
set NEWEXE=%~2

REM small delay to let old Koushin exit
ping 127.0.0.1 -n 3 >nul

:retryMove
move /Y "%NEWEXE%" "%EXE%" >nul 2>&1
if errorlevel 1 (
    REM file still locked? wait & retry
    ping 127.0.0.1 -n 2 >nul
    goto retryMove
)

REM start updated Koushin
start "" "%EXE%"

REM delete this updater script
del "%~f0"

endlocal
`

	if err := os.WriteFile(batPath, []byte(script), 0644); err != nil {
		return fmt.Errorf("write updater bat: %w", err)
	}

	cmd := exec.Command("cmd", "/c", batPath, exePath, newPath)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start updater: %w", err)
	}

	return nil
}

func checkForUpdatesInteractive(ctx context.Context, manual bool) {
	tag, url, err := fetchLatestRelease(ctx)
	if err != nil {
		if manual {
			messageBox("Koushin — Update check failed",
				"Could not check for updates:\n"+err.Error(),
				mbOK|mbIconError)
		}
		return
	}

	cur := normalizeVersion(appVersion)
	latest := normalizeVersion(tag)

	if !isNewerVersion(cur, latest) {
		if manual {
			messageBox("Koushin",
				fmt.Sprintf("You are up to date.\nCurrent version: %s\nLatest version: %s", cur, latest),
				mbOK|mbIconInfo)
		}
		return
	}

	if menuCheckUpdate != nil {
		menuCheckUpdate.SetTitle("Update available (" + latest + ")…")
	}

	var res int
	if !manual {
		res = messageBox("Koushin — Update available",
			fmt.Sprintf("A new version of Koushin is available.\n\nCurrent: %s\nLatest: %s\n\nUpdate now?", cur, latest),
			mbYesNo|mbIconQuestion)
		if res != idYes {
			return
		}
	} else {
		res = messageBox("Koushin — Update available",
			fmt.Sprintf("A new version of Koushin is available.\n\nCurrent: %s\nLatest: %s\n\nUpdate now?", cur, latest),
			mbYesNo|mbIconQuestion)
		if res != idYes {
			return
		}
	}

	uCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := performSelfUpdate(uCtx, tag, url); err != nil {
		messageBox("Koushin — Update failed",
			"Failed to update:\n"+err.Error(),
			mbOK|mbIconError)
		return
	}

	messageBox("Koushin — Updating",
		"Koushin will now close and restart with the updated version.",
		mbOK|mbIconInfo)

	os.Exit(0)
}

const (
	opHandshake = 0
	opFrame     = 1
	opClose     = 2
)

type discordIPC struct {
	conn io.ReadWriteCloser
	mu   sync.Mutex
}

type discordManager struct {
	mu          sync.Mutex
	ipc         *discordIPC
	lastAttempt time.Time
}

func (m *discordManager) ensureConnected(appID string) {
	defer logRecoveredPanic("discordManager.ensureConnected")

	if strings.TrimSpace(appID) == "" || appID == "MISSING_APP_ID" {
		return
	}
	m.mu.Lock()
	if m.ipc != nil {
		m.mu.Unlock()
		return
	}
	if !m.lastAttempt.IsZero() && time.Since(m.lastAttempt) < 2*time.Second {
		m.mu.Unlock()
		return
	}
	m.lastAttempt = time.Now()
	m.mu.Unlock()

	ipc, err := connectDiscordIPC(appID)
	if err != nil {
		return
	}

	m.mu.Lock()
	if m.ipc == nil {
		m.ipc = ipc
		ipc = nil
	}
	m.mu.Unlock()
	if ipc != nil {
		_ = ipc.close()
	}
}

func (m *discordManager) setActivity(appID string, activity map[string]any) {
	defer logRecoveredPanic("discordManager.setActivity")

	if strings.TrimSpace(appID) == "" || appID == "MISSING_APP_ID" {
		return
	}

	m.mu.Lock()
	ipc := m.ipc
	m.mu.Unlock()
	if ipc != nil {
		if err := ipc.setActivity(appID, activity); err == nil {
			return
		}
		m.mu.Lock()
		if m.ipc == ipc {
			m.ipc = nil
		}
		m.mu.Unlock()
		_ = ipc.close()
	}

	m.ensureConnected(appID)
	m.mu.Lock()
	ipc = m.ipc
	m.mu.Unlock()
	if ipc != nil {
		if err := ipc.setActivity(appID, activity); err != nil {
			m.mu.Lock()
			if m.ipc == ipc {
				m.ipc = nil
			}
			m.mu.Unlock()
			_ = ipc.close()
		}
	}
}

func (m *discordManager) close(appID string) {
	defer logRecoveredPanic("discordManager.close")

	m.mu.Lock()
	ipc := m.ipc
	m.ipc = nil
	m.mu.Unlock()
	if ipc != nil {
		_ = ipc.setActivity(appID, nil)
		_ = ipc.close()
	}
}

func connectDiscordIPCWithTimeout(appID string, dialTimeout time.Duration) (*discordIPC, error) {
	if dialTimeout <= 0 {
		dialTimeout = 500 * time.Millisecond
	}
	for i := 0; i < 10; i++ {
		path := fmt.Sprintf(`\\.\pipe\discord-ipc-%d`, i)
		conn, err := npipe.DialTimeout(path, dialTimeout)
		if err != nil {
			continue
		}
		ipc := &discordIPC{conn: conn}
		hello := map[string]any{"v": 1, "client_id": appID}
		if err := ipc.write(opHandshake, hello); err != nil {
			_ = conn.Close()
			continue
		}
		return ipc, nil
	}
	return nil, errors.New("could not connect to any discord-ipc pipe")
}

func connectDiscordIPC(appID string) (*discordIPC, error) {
	return connectDiscordIPCWithTimeout(appID, 350*time.Millisecond)
}

func (d *discordIPC) write(code uint32, payload any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:4], code)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(data)))
	if _, err := d.conn.Write(hdr); err != nil {
		return err
	}
	_, err = d.conn.Write(data)
	return err
}

func (d *discordIPC) close() error {
	_ = d.write(opClose, map[string]any{})
	return d.conn.Close()
}

func (d *discordIPC) setActivity(appID string, activity map[string]any) error {
	args := map[string]any{"pid": os.Getpid(), "activity": activity}
	envelope := map[string]any{
		"cmd": "SET_ACTIVITY", "args": args,
		"nonce": fmt.Sprintf("%d-%d", time.Now().UnixNano(), mrand.Int63()),
	}
	return d.write(opFrame, envelope)
}

type progSmooth struct {
	mu          sync.Mutex
	lastQueryAt time.Time
	lastPos     float64
	duration    float64
	paused      bool
	initialized bool
}

func (p *progSmooth) updateFromMPV(pos, dur float64, paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastQueryAt = time.Now()
	p.lastPos = pos
	if dur > 0 {
		p.duration = dur
	}
	p.paused = paused
	p.initialized = true
}

func (p *progSmooth) estimate() (pos, dur float64, paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos, dur, paused = p.lastPos, p.duration, p.paused
	if !p.initialized || p.lastQueryAt.IsZero() {
		return
	}
	if !paused && dur > 0 {
		elapsed := time.Since(p.lastQueryAt).Seconds()
		pos = p.lastPos + elapsed
		if pos > dur {
			pos = dur
		}
	}
	return
}

func (p *progSmooth) reset() {
	p.mu.Lock()
	p.lastQueryAt = time.Time{}
	p.lastPos = 0
	p.duration = 0
	p.paused = false
	p.initialized = false
	p.mu.Unlock()
}

func tsRange(now time.Time, cur, dur float64, paused bool) map[string]any {
	if paused || dur <= 0 || cur < 0 || cur > dur {
		return nil
	}
	start := now.Add(-time.Duration(cur) * time.Second).Unix()
	end := now.Add(time.Duration(dur-cur) * time.Second).Unix()
	return map[string]any{"start": start, "end": end}
}

func buildActivity(title, episode, _clock, coverURL, _smallKey, _aniURL string, cur, dur float64, paused bool) map[string]any {
	details := title
	epText := "Episode —"
	if strings.TrimSpace(episode) != "" {
		epText = "Episode " + episode
	}
	state := epText
	if paused {
		state = "Paused — " + epText
	}

	img := coverURL
	if strings.TrimSpace(img) == "" {
		img = fallbackLargeImageKey
	}

	assets := map[string]any{
		"large_image": img,
		"large_text":  title,
	}
	if _aniURL != "" {
		assets["large_url"] = _aniURL
	}

	store.mu.RLock()
	showProfile := store.ShowAniProfile && strings.TrimSpace(store.AccessToken) != ""
	username := store.Username
	store.mu.RUnlock()

	if _smallKey != "" && showProfile {
		assets["small_image"] = _smallKey
		name := strings.TrimSpace(username)
		if name == "" {
			name = "AniList user"
		}
		assets["small_text"] = name + " on AniList"
	}

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

type presenceCache struct {
	mu           sync.Mutex
	lastFile     string
	lastAni      string
	lastEp       string
	lastCover    string
	lastAniID    int
	lastTotalEps int
	startEpoch   time.Time
}

func (c *presenceCache) clear() {
	c.mu.Lock()
	c.lastFile = ""
	c.lastAni = ""
	c.lastEp = ""
	c.lastCover = ""
	c.lastAniID = 0
	c.lastTotalEps = 0
	c.startEpoch = time.Time{}
	c.mu.Unlock()
}

var (
	reEGeneric    = regexp.MustCompile(`(?i)\b(?:ep|eps|episode)\s*[-_. ]*\s*(\d{1,4})\b`)
	reDashEp      = regexp.MustCompile(`(?i)(?:^|\s)[-–—]\s*(\d{1,3})\b`)
	reEyyOnly     = regexp.MustCompile(`(?i)\bE(\d{1,3})\b`)
	reTrailingNum = regexp.MustCompile(`(?:^|[\s._-])(\d{1,3})(?:v\d)?\s*$`)
)

func pickEpisode(md *habari.Metadata, fallbackTitle string) (titleOut string, ep string) {
	titleOut = firstNonEmpty(md.FormattedTitle, md.Title, fallbackTitle)
	for _, arr := range [][]string{md.EpisodeNumber, md.EpisodeNumberAlt, md.OtherEpisodeNumber} {
		if len(arr) > 0 && strings.TrimSpace(arr[0]) != "" {
			ep = strings.TrimLeft(arr[0], "0")
			if ep == "" {
				ep = "0"
			}
			return
		}
	}
	if ep == "" {
		ep = guessEpisodeFromString(md.FileName)
	}
	if ep == "" {
		ep = guessEpisodeFromString(fallbackTitle)
	}
	return
}

func normalizeEpisodeCandidate(numStr string, allow4Digits bool) string {
	numStr = strings.TrimSpace(numStr)
	if numStr == "" {
		return ""
	}
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return ""
	}
	if n >= 1900 && n <= 2099 {
		return ""
	}
	switch n {
	case 2160, 1440, 1080, 720, 576, 480, 4320:
		return ""
	case 264, 265:
		return ""
	}
	if !allow4Digits && n >= 1000 {
		return ""
	}
	return strconv.Itoa(n)
}

func guessEpisodeFromString(s string) string {
	if s == "" {
		return ""
	}
	if ms := reEGeneric.FindAllStringSubmatch(s, -1); len(ms) > 0 {
		for _, m := range ms {
			if len(m) == 2 {
				if ep := normalizeEpisodeCandidate(m[1], true); ep != "" {
					return ep
				}
			}
		}
	}
	if m := reSxxEyy.FindStringSubmatch(s); len(m) == 3 {
		epDigits := strings.TrimSpace(m[2])
		if strings.TrimLeft(epDigits, "0") == "" {
			return "0"
		}
		if ep := normalizeEpisodeCandidate(epDigits, true); ep != "" {
			return ep
		}
	}
	if ms := reDashEp.FindAllStringSubmatch(s, -1); len(ms) > 0 {
		for _, m := range ms {
			if len(m) == 2 {
				if ep := normalizeEpisodeCandidate(m[1], false); ep != "" {
					return ep
				}
			}
		}
	}
	if m := reTrailingNum.FindStringSubmatch(s); len(m) == 2 {
		if ep := normalizeEpisodeCandidate(m[1], false); ep != "" {
			return ep
		}
	}
	return ""
}

const trayTitleBase = "Koushin"

var (
	menuQuit          *systray.MenuItem
	menuDiscordRPC    *systray.MenuItem
	menuLogin         *systray.MenuItem
	menuLogout        *systray.MenuItem
	menuCheckUpdate   *systray.MenuItem
	menuToggleProfile *systray.MenuItem
	menuFillerWarn    *systray.MenuItem
	menuCorrectAnime  *systray.MenuItem
	menuRunOnStartup  *systray.MenuItem
	menuSimul         *systray.MenuItem
	menuSimulHost     *systray.MenuItem
	menuSimulJoin     *systray.MenuItem
	menuSimulStop     *systray.MenuItem
	menuSimulStatus   *systray.MenuItem
	menuSimulJoinSync *systray.MenuItem

	loginCancelFunc context.CancelFunc
	loginCancelMu   sync.Mutex
)

type trackState struct {
	mu           sync.RWMutex
	Active       bool
	SeriesKey    string
	ParsedTitle  string
	MediaTitle   string
	WantYear     int
	WantSeason   int
	LastFileKey  string
	LastAniID    int
	LastTotalEps int
}

var curTrack trackState

func setCorrectionMenuActive(active bool) {
	if menuCorrectAnime == nil {
		return
	}
	if active {
		menuCorrectAnime.Enable()
	} else {
		menuCorrectAnime.Disable()
	}
}

func clearTrackingState() {
	curTrack.mu.Lock()
	curTrack.Active = false
	curTrack.SeriesKey = ""
	curTrack.ParsedTitle = ""
	curTrack.MediaTitle = ""
	curTrack.WantYear = 0
	curTrack.WantSeason = 0
	curTrack.LastFileKey = ""
	curTrack.LastAniID = 0
	curTrack.LastTotalEps = 0
	curTrack.mu.Unlock()
	setCorrectionMenuActive(false)
}

func traySetIdle() {
	systray.SetTooltip(trayTitleBase)
}

func traySetWatching(name, ep string, percent int, totalEps int) {
	name = strings.TrimSpace(name)
	ep = strings.TrimSpace(ep)
	if name == "" {
		systray.SetTooltip(trayTitleBase)
		return
	}

	epPart := ""
	if ep != "" {
		if totalEps > 0 {
			epPart = fmt.Sprintf(" · Ep %s/%d", ep, totalEps)
		} else {
			epPart = fmt.Sprintf(" · Ep %s/??", ep)
		}
	}

	pctPart := ""
	if percent >= 0 {
		pctPart = fmt.Sprintf(" · %d%%", percent)
	}

	tooltip := name + epPart + pctPart
	systray.SetTooltip(tooltip)
}

func traySetResolving(name, ep string, wait time.Duration, attempt int) {
	name = strings.TrimSpace(name)
	ep = strings.TrimSpace(ep)
	secs := int(wait.Seconds())
	base := name
	if base == "" {
		base = trayTitleBase
	}

	epPart := ""
	if ep != "" {
		epPart = " · Ep " + ep + "/??"
	}
	msg := fmt.Sprintf("%s%s · AniList rate limited, retrying in %ds (try %d)", base, epPart, secs, attempt)
	systray.SetTooltip(msg)
}

func refreshAuthMenu() {
	store.mu.RLock()
	hasToken := store.AccessToken != ""
	username := store.Username
	showAni := store.ShowAniProfile
	rpcOn := store.DiscordRPC
	warnFiller := store.WarnFiller
	store.mu.RUnlock()

	loginCancelMu.Lock()
	isLoggingIn := loginCancelFunc != nil
	loginCancelMu.Unlock()

	if isLoggingIn {
		return
	}

	if menuLogin == nil || menuLogout == nil {
		return
	}

	if hasToken {
		title := "AniList: Signed in"
		if username != "" {
			title = "Signed in as @" + username
		}
		menuLogin.SetTitle(title)
		menuLogin.Disable()
		menuLogout.Enable()

		if menuToggleProfile != nil {
			if rpcOn {
				menuToggleProfile.Enable()
			} else {
				menuToggleProfile.Disable()
			}
			if showAni {
				menuToggleProfile.Check()
			} else {
				menuToggleProfile.Uncheck()
			}
		}
	} else {
		menuLogin.SetTitle("Sign in to AniList…")
		menuLogin.Enable()
		menuLogout.Disable()

		if menuToggleProfile != nil {
			menuToggleProfile.Uncheck()
			menuToggleProfile.Disable()
		}
	}

	if menuDiscordRPC != nil {
		if rpcOn {
			menuDiscordRPC.Check()
		} else {
			menuDiscordRPC.Uncheck()
		}
	}

	if menuFillerWarn != nil {
		if warnFiller {
			menuFillerWarn.Check()
		} else {
			menuFillerWarn.Uncheck()
		}
	}
}

type authStore struct {
	Path           string
	AccessToken    string
	Username       string
	UserID         int
	ShowAniProfile bool
	DiscordRPC     bool `json:"discord_rpc"`
	WarnFiller     bool `json:"warn_filler"`
	RunOnStartup   bool `json:"run_on_startup"`
	SimulPort      int  `json:"simul_port"`
	SimulAutoUPnP  bool `json:"simul_auto_upnp"`
	SimulJoinSync  bool `json:"simul_join_sync"`
	mu             sync.RWMutex
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
	a.mu.Lock()
	defer a.mu.Unlock()
	b, err := os.ReadFile(a.file())
	if err != nil {
		return
	}
	var tmp struct {
		AccessToken    string `json:"access_token"`
		Username       string `json:"username"`
		UserID         int    `json:"user_id"`
		ShowAniProfile bool   `json:"show_ani_profile"`
		DiscordRPC     *bool  `json:"discord_rpc"`
		WarnFiller     bool   `json:"warn_filler"`
		RunOnStartup   bool   `json:"run_on_startup"`
		SimulPort      int    `json:"simul_port"`
		SimulAutoUPnP  *bool  `json:"simul_auto_upnp"`
		SimulJoinSync  bool   `json:"simul_join_sync"`
	}
	if json.Unmarshal(b, &tmp) == nil {
		a.AccessToken = tmp.AccessToken
		a.Username = tmp.Username
		a.UserID = tmp.UserID
		a.ShowAniProfile = tmp.ShowAniProfile
		if tmp.DiscordRPC == nil {
			a.DiscordRPC = true
		} else {
			a.DiscordRPC = *tmp.DiscordRPC
		}
		a.WarnFiller = tmp.WarnFiller
		a.RunOnStartup = tmp.RunOnStartup
		if validateSimulPort(tmp.SimulPort) {
			a.SimulPort = tmp.SimulPort
		} else {
			a.SimulPort = defaultSimulPort()
		}
		if tmp.SimulAutoUPnP == nil {
			a.SimulAutoUPnP = true
		} else {
			a.SimulAutoUPnP = *tmp.SimulAutoUPnP
		}
		a.SimulJoinSync = tmp.SimulJoinSync
	}
}

func (a *authStore) Save() {
	a.mu.RLock()
	data := struct {
		AccessToken    string `json:"access_token"`
		Username       string `json:"username"`
		UserID         int    `json:"user_id"`
		ShowAniProfile bool   `json:"show_ani_profile"`
		DiscordRPC     bool   `json:"discord_rpc"`
		WarnFiller     bool   `json:"warn_filler"`
		RunOnStartup   bool   `json:"run_on_startup"`
		SimulPort      int    `json:"simul_port"`
		SimulAutoUPnP  bool   `json:"simul_auto_upnp"`
		SimulJoinSync  bool   `json:"simul_join_sync"`
	}{
		a.AccessToken,
		a.Username,
		a.UserID,
		a.ShowAniProfile,
		a.DiscordRPC,
		a.WarnFiller,
		a.RunOnStartup,
		a.SimulPort,
		a.SimulAutoUPnP,
		a.SimulJoinSync,
	}
	a.mu.RUnlock()
	b, _ := json.MarshalIndent(data, "", "  ")
	_ = os.WriteFile(a.file(), b, 0600)
}

func (a *authStore) Clear() {
	a.mu.Lock()
	a.AccessToken = ""
	a.Username = ""
	a.UserID = 0
	a.ShowAniProfile = false
	a.DiscordRPC = true
	a.RunOnStartup = false
	a.SimulPort = 0
	a.SimulAutoUPnP = true
	a.SimulJoinSync = false
	a.mu.Unlock()
	_ = os.Remove(a.file())
}

var store = &authStore{Path: appDataDir(), WarnFiller: true, DiscordRPC: true}

type overrideStore struct {
	Path string
	mu   sync.RWMutex
	ByK  map[string]int `json:"by_key"`
}

func (o *overrideStore) file() string { return filepath.Join(o.Path, "overrides.json") }

func (o *overrideStore) Load() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ByK == nil {
		o.ByK = make(map[string]int)
	}
	b, err := os.ReadFile(o.file())
	if err != nil {
		return
	}
	var tmp struct {
		ByK map[string]int `json:"by_key"`
	}
	if json.Unmarshal(b, &tmp) == nil {
		if tmp.ByK != nil {
			o.ByK = tmp.ByK
		}
	}
}

func (o *overrideStore) Save() {
	o.mu.RLock()
	data := struct {
		ByK map[string]int `json:"by_key"`
	}{ByK: o.ByK}
	o.mu.RUnlock()
	b, _ := json.MarshalIndent(data, "", "  ")
	_ = os.WriteFile(o.file(), b, 0600)
}

func (o *overrideStore) Get(key string) int {
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.ByK == nil {
		return 0
	}
	return o.ByK[key]
}

func (o *overrideStore) Set(key string, mediaID int) {
	key = strings.TrimSpace(key)
	if key == "" || mediaID <= 0 {
		return
	}
	o.mu.Lock()
	if o.ByK == nil {
		o.ByK = make(map[string]int)
	}
	o.ByK[key] = mediaID
	o.mu.Unlock()
	o.Save()
}

var overrides = &overrideStore{Path: appDataDir()}

func mediaLiteTitle(m mediaLite) string {
	name := strings.TrimSpace(m.Title.English)
	if name == "" {
		name = firstNonEmpty(m.Title.Romaji, m.Title.Native)
	}
	return strings.TrimSpace(name)
}

func startAniListSelector(initialQuery string, ua string, onSelect func(id int)) error {
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
<html><head><meta charset="utf-8"><title>Koushin · Select anime</title>
<style>
body{font-family:system-ui;max-width:900px;margin:24px auto;line-height:1.4}
input{width:100%;padding:10px;font-size:16px}
.row{display:flex;gap:12px;align-items:center;padding:10px;border-bottom:1px solid #eee}
button{padding:6px 10px}
small{color:#666}
</style></head>
<body>
<h2>Select correct anime</h2>
<p>Search AniList and click <b>Select</b>.</p>
<input id="q" placeholder="Search…" />
<div id="status"><small></small></div>
<div id="results"></div>
<script>
const q = document.getElementById('q');
const results = document.getElementById('results');
const statusEl = document.querySelector('#status small');

function esc(s){return (s||'').replace(/[&<>\"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]));}

async function search() {
  const term = q.value.trim();
  if (!term) { results.innerHTML=''; statusEl.textContent=''; return; }
  statusEl.textContent = 'Searching…';
  const res = await fetch('/api/search?q=' + encodeURIComponent(term));
  const data = await res.json();
  if (!res.ok) { statusEl.textContent = data.error || 'Search failed'; return; }
  statusEl.textContent = 'Results: ' + data.results.length;
  results.innerHTML = data.results.map(m => {
    const meta = [m.format, m.year ? ('year ' + m.year) : '', m.episodes ? (m.episodes + ' eps') : ''].filter(Boolean).join(' · ');
    return '<div class="row">'
      + '<div style="flex:1">'
        + '<div><b>' + esc(m.title) + '</b></div>'
        + '<div><small>' + esc(meta) + ' · id ' + m.id + '</small></div>'
      + '</div>'
      + '<button onclick="selectID(' + m.id + ')">Select</button>'
    + '</div>';
  }).join('');
}

async function selectID(id) {
  statusEl.textContent = 'Saving…';
  const res = await fetch('/api/select', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({id})});
  const data = await res.json();
  if (!res.ok) { statusEl.textContent = data.error || 'Failed'; return; }
  // Try to close the tab. Some browsers will block window.close() for tabs
  // not opened by script; in that case we navigate to /done which tries again.
  statusEl.textContent = 'Saved. Closing…';
  try { window.open('', '_self'); window.close(); } catch (e) {}
  setTimeout(() => { try { window.open('', '_self'); window.close(); } catch (e) {} }, 250);
  window.location.replace('/done');
}

q.addEventListener('input', () => { clearTimeout(window._t); window._t = setTimeout(search, 250); });

const params = new URLSearchParams(window.location.search);
const init = params.get('q');
if (init) { q.value = init; search(); }
</script>
</body></html>`)
	})

	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Koushin · Done</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Saved</h2>
<p>Attempting to close this tab…</p>
<script>
setTimeout(() => {
  try { window.open('', '_self'); window.close(); } catch (e) {}
}, 100);
</script>
<p><small>If it doesn't close automatically, you can close it now.</small></p>
</body></html>`)
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "empty query"})
			return
		}
		c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ms, err := searchAniList(c, q, ua)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		type row struct {
			ID       int    `json:"id"`
			Title    string `json:"title"`
			Year     int    `json:"year"`
			Format   string `json:"format"`
			Episodes int    `json:"episodes"`
		}
		rows := make([]row, 0, len(ms))
		for _, m := range ms {
			rows = append(rows, row{ID: m.ID, Title: mediaLiteTitle(m), Year: m.StartDate.Year, Format: m.Format, Episodes: m.Episodes})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": rows})
	})

	selected := make(chan int, 1)
	mux.HandleFunc("/api/select", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "method not allowed"})
			return
		}
		var body struct {
			ID int `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid id"})
			return
		}
		select {
		case selected <- body.ID:
		default:
		}
		if onSelect != nil {
			onSelect(body.ID)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	go func() {
		_ = srv.Serve(ln)
	}()

	go func() {
		select {
		case <-selected:
			time.Sleep(5 * time.Second)
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = srv.Shutdown(c)
			cancel()
		case <-time.After(5 * time.Minute):
			c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = srv.Shutdown(c)
			cancel()
		}
	}()

	url := baseURL
	if strings.TrimSpace(initialQuery) != "" {
		url += "?q=" + urlQueryEscape(initialQuery)
	}
	openBrowser(url)
	return nil
}

func urlQueryEscape(s string) string {
	repl := strings.NewReplacer(
		"%", "%25",
		" ", "%20",
		"+", "%2B",
		"&", "%26",
		"?", "%3F",
		"#", "%23",
		"=", "%3D",
	)
	return repl.Replace(s)
}

func openBrowser(u string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}

func oauthLogin(ctx context.Context) error {
	clientID := anilistClientID

	tokenCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Addr: localLoginAddr}
	mux := http.NewServeMux()

	receivedResponse := false
	var responseMu sync.Mutex

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		responseMu.Lock()
		receivedResponse = true
		responseMu.Unlock()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Koushin · AniList Login</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Completing login...</h2>
<p id="status">Processing authentication...</p>
<script>
(function() {
	// The access token is in the URL fragment (after #)
	const hash = window.location.hash.substring(1); // Remove the leading #
	
	if (!hash) {
		document.getElementById('status').textContent = 'No authentication data found. Please try again.';
		return;
	}
	
	const params = new URLSearchParams(hash);
	const token = params.get('access_token');
	const error = params.get('error');
	
	if (error) {
		document.getElementById('status').textContent = 'Authentication failed: ' + error;
		fetch('/submit', {
			method: 'POST',
			headers: {'Content-Type': 'application/json'},
			body: JSON.stringify({error: error})
		});
		return;
	}
	
	if (token) {
		document.getElementById('status').textContent = 'Login successful! Closing...';
		fetch('/submit', {
			method: 'POST',
			headers: {'Content-Type': 'application/json'},
			body: JSON.stringify({token: token})
		}).then(response => {
			if (response.ok) {
				document.getElementById('status').textContent = 'Login successful! You can close this window.';
				setTimeout(() => window.close(), 1500);
			} else {
				document.getElementById('status').textContent = 'Login failed. Please try again.';
			}
		}).catch(err => {
			document.getElementById('status').textContent = 'Error: ' + err.message;
		});
	} else {
		document.getElementById('status').textContent = 'No access token found. Please try again.';
	}
})();
</script>
</body></html>`)
	})

	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var body struct {
			Token string `json:"token"`
			Error string `json:"error"`
		}

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			errCh <- fmt.Errorf("failed to decode response: %w", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		if body.Error != "" {
			errCh <- fmt.Errorf("anilist oauth error: %s", body.Error)
			http.Error(w, "authentication error", http.StatusBadRequest)
			return
		}

		tok := strings.TrimSpace(body.Token)
		if tok == "" {
			errCh <- errors.New("empty access token")
			http.Error(w, "empty token", http.StatusBadRequest)
			return
		}

		tokenCh <- tok
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	srv.Handler = mux
	go func() { _ = srv.ListenAndServe() }()
	defer func() {
		c2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(c2)
		cancel()
	}()

	authURL := fmt.Sprintf("https://anilist.co/api/v2/oauth/authorize?client_id=%s&response_type=token",
		clientID)

	logAppend("oauth(implicit): opening ", authURL)
	openBrowser(authURL)

	loginTimeout := 2 * time.Minute
	checkInterval := 5 * time.Second
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	timeoutTimer := time.NewTimer(loginTimeout)
	defer timeoutTimer.Stop()

	var accessToken string
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-errCh:
			return err

		case accessToken = <-tokenCh:
			if strings.TrimSpace(accessToken) == "" {
				return errors.New("empty access token")
			}
			logAppend("oauth(implicit): got token len=", fmt.Sprint(len(accessToken)))
			goto success

		case <-timeoutTimer.C:
			responseMu.Lock()
			gotResponse := receivedResponse
			responseMu.Unlock()

			if !gotResponse {
				logAppend("oauth(implicit): timeout - user likely closed the browser")
				return errors.New("login cancelled or timed out")
			}
			return errors.New("authentication timeout")

		case <-ticker.C:
		}
	}

success:
	name, uid, whoErr := whoAmI(ctx, accessToken)
	if whoErr != nil {
		logAppend("oauth: whoAmI error: ", whoErr.Error())
		return fmt.Errorf("failed to verify token: %w", whoErr)
	}

	store.mu.Lock()
	store.AccessToken = accessToken
	store.Username = name
	store.UserID = uid
	store.ShowAniProfile = true
	store.mu.Unlock()
	store.Save()

	logAppend("oauth: success; logged in as ", name, " (id ", fmt.Sprint(uid), ")")
	return nil
}

func whoAmI(ctx context.Context, token string) (username string, userID int, err error) {
	const q = `query{ Viewer { id name } }`
	body := map[string]any{"query": q}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("whoAmI http %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Data struct {
			Viewer struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"Viewer"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) == nil {
		return out.Data.Viewer.Name, out.Data.Viewer.ID, nil
	}
	return "", 0, errors.New("decode viewer failed")
}

type mediaListEntryLite struct {
	ID       int    `json:"id"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
}

func getMediaListEntry(ctx context.Context, token string, userID int, mediaID int) (entry *mediaListEntryLite, err error) {
	const q = `
query($userId:Int, $mediaId:Int) {
  MediaList(userId:$userId, mediaId:$mediaId, type: ANIME) {
    id
    status
    progress
  }
}`
	vars := map[string]any{"userId": userID, "mediaId": mediaID}
	body := map[string]any{"query": q, "variables": vars}
	bs, _ := json.Marshal(body)

	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("MediaList http %d: %s", resp.StatusCode, string(b))
	}

	var out struct {
		Data struct {
			MediaList *mediaListEntryLite `json:"MediaList"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data.MediaList, nil
}

func saveMediaListEntry(ctx context.Context, token string, mediaID int, progress int, status string) error {
	const m = `
mutation($mediaId:Int, $progress:Int, $status:MediaListStatus) {
  SaveMediaListEntry(mediaId:$mediaId, progress:$progress, status:$status) { id status progress }
}`
	vars := map[string]any{"mediaId": mediaID, "progress": progress, "status": status}
	body := map[string]any{"query": m, "variables": vars}
	bs, _ := json.Marshal(body)

	req, _ := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("SaveMediaListEntry http %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func syncAniListProgress(ctx context.Context, token string, userID int, mediaID int, progress int, totalEps int) error {
	if strings.TrimSpace(token) == "" || userID <= 0 || mediaID <= 0 || progress <= 0 {
		return errors.New("invalid sync parameters")
	}
	if totalEps == 1 && progress > 1 {
		progress = 1
	}
	if totalEps > 0 && progress > totalEps {
		progress = totalEps
	}

	var existingStatus string
	if e, err := getMediaListEntry(ctx, token, userID, mediaID); err == nil && e != nil {
		existingStatus = strings.TrimSpace(e.Status)
	}

	targetStatus := aniStatusCurrent
	switch strings.ToUpper(existingStatus) {
	case aniStatusCompleted:
		targetStatus = aniStatusRepeating
	case aniStatusRepeating:
		targetStatus = aniStatusRepeating
	case aniStatusCurrent:
		targetStatus = aniStatusCurrent
	case "":
		targetStatus = aniStatusCurrent
	default:
		targetStatus = aniStatusCurrent
	}

	if totalEps > 0 && progress >= totalEps {
		targetStatus = aniStatusCompleted
	}

	return saveMediaListEntry(ctx, token, mediaID, progress, targetStatus)
}

func refreshViewerFromToken(ctx context.Context) {
	store.mu.RLock()
	tok := strings.TrimSpace(store.AccessToken)
	store.mu.RUnlock()
	if tok == "" {
		return
	}
	c, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	name, uid, err := whoAmI(c, tok)
	if err != nil {
		logAppend("viewer refresh failed: ", err.Error())
		return
	}

	store.mu.Lock()
	changed := store.Username != name || store.UserID != uid
	store.Username = name
	store.UserID = uid
	store.mu.Unlock()
	if changed {
		store.Save()
		refreshAuthMenu()
	}
}

func onReadyTray(ctx context.Context, cancel context.CancelFunc) {
	setTrayIcon()
	systray.SetTooltip("Koushin")

	menuDiscordRPC = systray.AddMenuItemCheckbox(
		"Enable Discord Rich Presence",
		"Toggle Discord Rich Presence updates (tray + AniList still work)",
		store.DiscordRPC,
	)
	menuToggleProfile = systray.AddMenuItemCheckbox(
		"Show AniList profile in Discord RPC",
		"Toggle AniList profile small icon",
		store.ShowAniProfile,
	)
	systray.AddSeparator()

	menuLogin = systray.AddMenuItem("Sign in to AniList…", "Authenticate this device with AniList")
	menuLogout = systray.AddMenuItem("Sign out of AniList", "Forget saved AniList token")
	menuCorrectAnime = systray.AddMenuItem("Select correct anime…", "Manually select the AniList anime for the current file")
	menuCorrectAnime.Disable()
	menuFillerWarn = systray.AddMenuItemCheckbox(
		"Warn for filler episodes",
		"Show a warning when watching filler episodes (when available)",
		store.WarnFiller,
	)
	systray.AddSeparator()

	menuSimul = systray.AddMenuItem("Simulwatching", "Host or join a Simulwatching session")
	menuSimulHost = menuSimul.AddSubMenuItem("Host a Simul…", "Host a session and share your watch state")
	menuSimulJoin = menuSimul.AddSubMenuItem("Join a Simul…", "Join a friend's hosted session")
	menuSimulStop = menuSimul.AddSubMenuItem("Stop Simulwatching", "Stop hosting/joining")
	menuSimulStatus = menuSimul.AddSubMenuItem("Status: Off", "Shows current Simulwatching state")
	menuSimulStatus.Disable()
	store.mu.RLock()
	joinSync := store.SimulJoinSync
	store.mu.RUnlock()
	menuSimulJoinSync = menuSimul.AddSubMenuItemCheckbox(
		"Sync my AniList",
		"When joined, sync your own AniList based on the host's 80% progress events",
		joinSync,
	)
	simulLoadSettingsFromStore()
	simulRefreshTray()
	systray.AddSeparator()

	menuRunOnStartup = systray.AddMenuItemCheckbox(
		"Run on Windows startup",
		"Automatically start Koushin when you sign in to Windows",
		store.RunOnStartup,
	)
	systray.AddSeparator()

	menuCheckUpdate = systray.AddMenuItem("Check for updates…", "Check if a newer Koushin version is available")
	systray.AddSeparator()
	menuQuit = systray.AddMenuItem("Quit", "Exit Koushin")

	refreshAuthMenu()

	go refreshViewerFromToken(ctx)

	go func() {
		for {
			select {
			case <-menuDiscordRPC.ClickedCh:
				store.mu.Lock()
				store.DiscordRPC = !store.DiscordRPC
				on := store.DiscordRPC
				store.mu.Unlock()
				if on {
					menuDiscordRPC.Check()
				} else {
					menuDiscordRPC.Uncheck()
					go func() {
						cfg2 := loadConfig()
						m2 := &discordManager{}
						m2.setActivity(cfg2.DiscordAppID, nil)
						m2.close(cfg2.DiscordAppID)
					}()
				}
				store.Save()
				refreshAuthMenu()

			case <-menuLogin.ClickedCh:
				loginCancelMu.Lock()
				if loginCancelFunc != nil {
					loginCancelFunc()
					loginCancelFunc = nil
					loginCancelMu.Unlock()

					menuLogin.SetTitle("Sign in to AniList…")
					menuLogin.Enable()
					continue
				}
				loginCancelMu.Unlock()

				menuLogin.SetTitle("Cancel login…")

				loginCtx, loginCancel := context.WithCancel(ctx)
				loginCancelMu.Lock()
				loginCancelFunc = loginCancel
				loginCancelMu.Unlock()

				go func() {
					err := oauthLogin(loginCtx)

					loginCancelMu.Lock()
					loginCancelFunc = nil
					loginCancelMu.Unlock()

					if err != nil {
						if err == context.Canceled {
							fmt.Println("AniList login cancelled by user")
							logAppend("AniList login cancelled by user")
						} else {
							fmt.Println("AniList login failed:", err)
							logAppend("AniList login failed:", err.Error())
						}
					} else {
						fmt.Println("AniList login succeeded")
					}

					store.Load()
					refreshAuthMenu()
				}()

			case <-menuLogout.ClickedCh:
				store.Clear()
				refreshAuthMenu()

			case <-menuToggleProfile.ClickedCh:
				store.mu.Lock()
				if !store.DiscordRPC {
					store.ShowAniProfile = false
					store.mu.Unlock()
					menuToggleProfile.Uncheck()
					menuToggleProfile.Disable()
					store.Save()
					continue
				}
				if store.AccessToken == "" {
					store.ShowAniProfile = false
					store.mu.Unlock()
					menuToggleProfile.Uncheck()
					menuToggleProfile.Disable()
					continue
				}
				store.ShowAniProfile = !store.ShowAniProfile
				newVal := store.ShowAniProfile
				store.mu.Unlock()
				store.Save()

				if newVal {
					menuToggleProfile.Check()
				} else {
					menuToggleProfile.Uncheck()
				}

			case <-menuFillerWarn.ClickedCh:
				store.mu.Lock()
				store.WarnFiller = !store.WarnFiller
				on := store.WarnFiller
				store.mu.Unlock()
				if on {
					menuFillerWarn.Check()
				} else {
					menuFillerWarn.Uncheck()
				}
				store.Save()

			case <-menuRunOnStartup.ClickedCh:
				store.mu.Lock()
				store.RunOnStartup = !store.RunOnStartup
				on := store.RunOnStartup
				store.mu.Unlock()
				if on {
					menuRunOnStartup.Check()
				} else {
					menuRunOnStartup.Uncheck()
				}
				store.Save()
				if err := setWindowsRunOnStartup(on); err != nil {
					logAppend("startup: failed to update HKCU Run entry: ", err.Error())
					messageBox("Koushin — Startup setting failed", "Could not update Windows startup setting:\n\n"+err.Error(), mbOK|mbIconError)
				}

			case <-menuSimulJoinSync.ClickedCh:
				store.mu.Lock()
				store.SimulJoinSync = !store.SimulJoinSync
				on := store.SimulJoinSync
				store.mu.Unlock()
				if on {
					menuSimulJoinSync.Check()
				} else {
					menuSimulJoinSync.Uncheck()
				}
				store.Save()

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

			case <-menuCorrectAnime.ClickedCh:
				curTrack.mu.RLock()
				active := curTrack.Active
				seriesKey := curTrack.SeriesKey
				q := cleanTitleForSearch(firstNonEmpty(curTrack.ParsedTitle, curTrack.MediaTitle))
				curTrack.mu.RUnlock()
				if !active || seriesKey == "" {
					continue
				}
				go func(seriesKey string, initialQ string) {
					_ = startAniListSelector(initialQ, loadConfig().UserAgent, func(id int) {
						overrides.Set(seriesKey, id)
						select {
						case resolveCh <- resolveRequest{SeriesKey: seriesKey, MediaID: id}:
						default:
						}
					})
				}(seriesKey, q)

			case <-menuCheckUpdate.ClickedCh:
				go func() {
					c, cancel2 := context.WithTimeout(ctx, 30*time.Second)
					defer cancel2()
					checkForUpdatesInteractive(c, true)
				}()

			case <-menuQuit.ClickedCh:
				loginCancelMu.Lock()
				if loginCancelFunc != nil {
					loginCancelFunc()
					loginCancelFunc = nil
				}
				loginCancelMu.Unlock()

				cancel()
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		time.Sleep(5 * time.Second)
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		checkForUpdatesInteractive(c, false)
	}()
}

func runWithTray(mainfn func(context.Context), onExit func()) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	systray.Run(func() {
		onReadyTray(ctx, cancel)

		go func() {
			defer cancel()
			mainfn(ctx)
		}()

		go func() {
			<-ctx.Done()
			systray.Quit()
		}()
	}, func() {
		if onExit != nil {
			onExit()
		}
	})
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

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

const (
	mbOK           = 0x00000000
	mbOKCancel     = 0x00000001
	mbYesNo        = 0x00000004
	mbIconInfo     = 0x00000040
	mbIconQuestion = 0x00000020
	mbIconError    = 0x00000010

	mbSystemModal   = 0x00001000
	mbSetForeground = 0x00010000
	mbTopMost       = 0x00040000

	idOK  = 1
	idYes = 6
)

func messageBox(title, text string, style uint32) int {
	style |= mbSetForeground | mbTopMost

	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(title)
	r, _, _ := procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(t)),
		uintptr(unsafe.Pointer(c)),
		uintptr(style),
	)
	return int(r)
}

func main() {
	firstRun := !fileExists(store.file())
	ln, err := net.Listen("tcp", "127.0.0.1:45222")
	if err != nil {
		return
	}
	defer ln.Close()

	cfg := loadConfig()
	store.Load()
	loadZeroBasedSignatures()
	if firstRun {
		store.mu.Lock()
		store.RunOnStartup = true
		on := store.RunOnStartup
		store.mu.Unlock()
		store.Save()
		if err := setWindowsRunOnStartup(on); err != nil {
			logAppend("startup: failed to enable auto-start on first run: ", err.Error())
		}
		if err := ensureStartMenuShortcut(); err != nil {
			logAppend("startmenu: failed to create shortcut: ", err.Error())
		}
	} else {
		store.mu.RLock()
		on := store.RunOnStartup
		store.mu.RUnlock()
		if err := setWindowsRunOnStartup(on); err != nil {
			logAppend("startup: failed to apply auto-start setting: ", err.Error())
		}
		if err := ensureStartMenuShortcut(); err != nil {
			logAppend("startmenu: failed to ensure shortcut: ", err.Error())
		}
	}
	overrides.Load()
	runWithTray(func(ctx context.Context) { run(ctx, cfg) }, nil)
}

type fillerInfo struct {
	Slug       string
	FillerEps  map[int]bool
	LastLookup time.Time
}

var (
	fillerCache = struct {
		mu      sync.Mutex
		byAniID map[int]*fillerInfo
	}{
		byAniID: make(map[int]*fillerInfo),
	}
	fillerWarned = struct {
		mu  sync.Mutex
		set map[string]bool
	}{
		set: make(map[string]bool),
	}
	fillerRowRe = regexp.MustCompile(`(?s)<tr class="filler[^"]*"[^>]*>.*?<td class="Number">(\d+)</td>`)
)

func levenshtein(a, b string) int {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	na, nb := len(a), len(b)
	if na == 0 {
		return nb
	}
	if nb == 0 {
		return na
	}
	dp := make([]int, (na+1)*(nb+1))
	idx := func(i, j int) int { return i*(nb+1) + j }

	for i := 0; i <= na; i++ {
		dp[idx(i, 0)] = i
	}
	for j := 0; j <= nb; j++ {
		dp[idx(0, j)] = j
	}

	for i := 1; i <= na; i++ {
		for j := 1; j <= nb; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			del := dp[idx(i-1, j)] + 1
			ins := dp[idx(i, j-1)] + 1
			sub := dp[idx(i-1, j-1)] + cost
			dp[idx(i, j)] = minInt(del, minInt(ins, sub))
		}
	}
	return dp[idx(na, nb)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type fillerSearchResult struct {
	Slug  string
	Title string
}

func fetchFillerShows(ctx context.Context) ([]fillerSearchResult, error) {
	results := []fillerSearchResult{}

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.animefillerlist.com/shows", nil)
	req.Header.Set("User-Agent", "koushin-filler-check/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("animefillerlist shows http %d: %s", resp.StatusCode, string(body))
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	htmlStr := string(b)

	needle := `<a href="/shows/`
	for {
		i := strings.Index(htmlStr, needle)
		if i == -1 {
			break
		}
		htmlStr = htmlStr[i+len(`<a href="`):]
		j := strings.Index(htmlStr, `"`)
		if j == -1 {
			break
		}
		href := htmlStr[:j]
		htmlStr = htmlStr[j+2:]

		k := strings.Index(htmlStr, "</a>")
		if k == -1 {
			break
		}
		title := htmlStr[:k]
		title = html.UnescapeString(strings.TrimSpace(title))

		if strings.HasPrefix(href, "/shows/") && title != "" {
			results = append(results, fillerSearchResult{
				Slug:  href,
				Title: title,
			})
		}

		htmlStr = htmlStr[k+4:]
	}

	if len(results) == 0 {
		return nil, errors.New("no shows parsed from animefillerlist")
	}
	return results, nil
}

func searchFillerSlug(ctx context.Context, titles []string) (string, error) {
	shows, err := fetchFillerShows(ctx)
	if err != nil {
		return "", err
	}

	type candidate struct {
		Slug     string
		Title    string
		Distance int
	}

	var cands []candidate
	for _, s := range shows {
		baseTitle := s.Title

		secondTitle := ""
		if start := strings.LastIndex(baseTitle, " ("); start != -1 && strings.HasSuffix(baseTitle, ")") {
			secondTitle = baseTitle[start+2 : len(baseTitle)-1]
			baseTitle = baseTitle[:start]
		}

		allVariants := []string{baseTitle}
		if secondTitle != "" {
			allVariants = append(allVariants, secondTitle)
		}

		for _, t := range titles {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			for _, v := range allVariants {
				dist := levenshtein(t, v)
				cands = append(cands, candidate{
					Slug:     s.Slug,
					Title:    v,
					Distance: dist,
				})
			}
		}
	}

	if len(cands) == 0 {
		return "", errors.New("no candidates for filler slug")
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Distance != cands[j].Distance {
			return cands[i].Distance < cands[j].Distance
		}
		return len(cands[i].Title) < len(cands[j].Title)
	})

	best := cands[0]
	if best.Distance > 10 {
		return "", errors.New("no good filler match (distance too high)")
	}

	return best.Slug, nil
}

func fetchFillerEpisodes(ctx context.Context, slug string) (map[int]bool, error) {
	if !strings.HasPrefix(slug, "/") {
		slug = "/shows/" + slug
	}
	fullURL := "https://www.animefillerlist.com" + slug

	req, _ := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	req.Header.Set("User-Agent", "koushin-filler-check/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("animefillerlist show http %d: %s", resp.StatusCode, string(body))
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	htmlStr := string(b)

	matches := fillerRowRe.FindAllStringSubmatch(htmlStr, -1)
	filler := make(map[int]bool)

	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		numStr := strings.TrimSpace(m[1])
		if n, err := strconv.Atoi(numStr); err == nil && n > 0 {
			filler[n] = true
		}
	}

	if len(filler) == 0 {
		return nil, errors.New("no filler episodes parsed from page")
	}
	return filler, nil
}

func getFillerInfo(ctx context.Context, aniID int, titles []string) (*fillerInfo, error) {
	fillerCache.mu.Lock()
	if fi, ok := fillerCache.byAniID[aniID]; ok && fi.FillerEps != nil {
		fillerCache.mu.Unlock()
		return fi, nil
	}
	fillerCache.mu.Unlock()

	slug, err := searchFillerSlug(ctx, titles)
	if err != nil {
		return nil, err
	}
	eps, err := fetchFillerEpisodes(ctx, slug)
	if err != nil {
		return nil, err
	}

	fi := &fillerInfo{
		Slug:       slug,
		FillerEps:  eps,
		LastLookup: time.Now(),
	}
	fillerCache.mu.Lock()
	fillerCache.byAniID[aniID] = fi
	fillerCache.mu.Unlock()
	return fi, nil
}

func markFillerWarned(aniID, ep int) bool {
	key := fmt.Sprintf("%d:%d", aniID, ep)
	fillerWarned.mu.Lock()
	defer fillerWarned.mu.Unlock()
	if fillerWarned.set[key] {
		return false
	}
	fillerWarned.set[key] = true
	return true
}

func fallbackTitleFrom(st mpvState) string {
	if t := strings.TrimSpace(st.MediaTitle); t != "" {
		return t
	}
	base := filepath.Base(strings.TrimSpace(st.FileName))
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return base
}

func run(ctx context.Context, cfg Config) {
	mgr := &discordManager{}
	defer mgr.close(cfg.DiscordAppID)

	if !discordRPCEnabled() {
		discordApplyActivity(cfg.DiscordAppID, mgr, nil)
	}

	joined := func() bool {
		simul.mu.Lock()
		on := simul.mode == simulModeJoin
		simul.mu.Unlock()
		return on
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if joined() {
			simulApplyJoinStateToUI(cfg, mgr)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		mpvConn, err := npipe.DialTimeout(cfg.MpvPipe, 2*time.Second)
		if err != nil {
			discordApplyActivity(cfg.DiscordAppID, mgr, nil)
			traySetIdle()
			time.Sleep(1500 * time.Millisecond)
			continue
		}

		runLoop(ctx, cfg, mpvConn, mgr)
		traySetIdle()
		time.Sleep(1 * time.Second)
	}
}

func runLoop(ctx context.Context, cfg Config, conn net.Conn, mgr *discordManager) {
	defer conn.Close()

	cache := &presenceCache{}
	smooth := &progSmooth{}
	mpvTicker := time.NewTicker(cfg.PollInterval)
	uiTicker := time.NewTicker(1 * time.Second)
	var lastMpvUpdateAt time.Time
	defer mpvTicker.Stop()
	defer uiTicker.Stop()

	var currentFileKey string
	playing := false
	ready := false
	clearTrackingState()

	setPresence := func(details map[string]any) {
		discordApplyActivity(cfg.DiscordAppID, mgr, details)
	}

	joined := func() bool {
		simul.mu.Lock()
		on := simul.mode == simulModeJoin
		simul.mu.Unlock()
		return on
	}
	hosting := func() bool {
		simul.mu.Lock()
		on := simul.mode == simulModeHost
		simul.mu.Unlock()
		return on
	}
	setHostIdle := func() {
		if !hosting() {
			return
		}
		st := simulState{Ready: false, UpdatedAt: time.Now().UnixNano()}
		simul.mu.Lock()
		simul.hostState = st
		simul.mu.Unlock()
		simulBroadcastState(st)
	}
	broadcastHostState := func(hostFile string) {
		if !hosting() {
			return
		}
		cache.mu.Lock()
		aname := cache.lastAni
		ep := cache.lastEp
		cover := cache.lastCover
		aniID := cache.lastAniID
		totalEps := cache.lastTotalEps
		cache.mu.Unlock()

		var pos, dur float64
		var paused bool
		if ready {
			pos, dur, paused = smooth.estimate()
		} else {
			_, dur, paused = smooth.estimate()
			pos = 0
		}

		curTrack.mu.RLock()
		hs := curTrack.WantSeason
		curTrack.mu.RUnlock()

		st := simulState{
			Anime:      strings.TrimSpace(aname),
			Episode:    strings.TrimSpace(ep),
			AniID:      aniID,
			TotalEps:   totalEps,
			CoverURL:   strings.TrimSpace(cover),
			Paused:     paused,
			Pos:        pos,
			Dur:        dur,
			Ready:      ready && playing,
			UpdatedAt:  time.Now().UnixNano(),
			HostFile:   strings.TrimSpace(hostFile),
			HostSeason: hs,
		}
		simul.mu.Lock()
		simul.hostState = st
		simul.mu.Unlock()
		simulBroadcastState(st)
	}

	pushEstimated := func() {
		if !playing {
			return
		}
		cache.mu.Lock()
		aname := cache.lastAni
		ep := cache.lastEp
		cover := cache.lastCover
		aniID := cache.lastAniID
		totalEps := cache.lastTotalEps
		cache.mu.Unlock()

		var pos, dur float64
		var paused bool
		if ready {
			pos, dur, paused = smooth.estimate()
		} else {
			_, dur, paused = smooth.estimate()
			pos = 0
		}

		episodeLabel := ""
		if strings.TrimSpace(ep) != "" {
			if totalEps > 0 {
				episodeLabel = fmt.Sprintf("%s/%d", ep, totalEps)
			} else {
				episodeLabel = fmt.Sprintf("%s/??", ep)
			}
		}

		act := buildActivity(aname, episodeLabel, "", cover, cfg.SmallImage, anilistURL(aniID), pos, dur, paused)
		if !ready {
			delete(act, "timestamps")
		}
		setPresence(act)

		percent := -1
		if dur > 0 {
			p := int(pos/dur*100 + 0.5)
			if p < 0 {
				p = 0
			} else if p > 100 {
				p = 100
			}
			percent = p
		}
		traySetWatching(aname, ep, percent, totalEps)
	}

	reported := make(map[string]bool)

	for {
		select {
		case <-ctx.Done():
			return

		case rr := <-resolveCh:
			curTrack.mu.RLock()
			active := curTrack.Active
			seriesKey := curTrack.SeriesKey
			curTrack.mu.RUnlock()
			if active && seriesKey != "" && rr.SeriesKey == seriesKey {
				currentFileKey = ""
			}

		case <-mpvTicker.C:
			if joined() {
				simulApplyJoinStateToUI(cfg, mgr)
				continue
			}
			st, err := queryMpvState(conn)
			if err != nil {
				if strings.Contains(err.Error(), "no file playing") {
					if !mpvAlive(conn) {
						setHostIdle()
						playing = false
						ready = false
						currentFileKey = ""
						smooth.reset()
						cache.clear()
						clearTrackingState()
						setPresence(nil)
						traySetIdle()
						return
					}

					setHostIdle()
					playing = false
					ready = false
					currentFileKey = ""
					smooth.reset()
					cache.clear()
					clearTrackingState()
					setPresence(nil)
					traySetIdle()
					continue
				}

				if isPipeGone(err) {
					fmt.Println("mpv pipe closed, reconnecting...")
					setHostIdle()
					playing = false
					ready = false
					currentFileKey = ""
					smooth.reset()
					cache.clear()
					clearTrackingState()
					setPresence(nil)
					traySetIdle()
					return
				}

				fmt.Println("mpv error:", err)
				setHostIdle()
				playing = false
				ready = false
				currentFileKey = ""
				smooth.reset()
				cache.clear()
				clearTrackingState()
				setPresence(nil)
				traySetIdle()
				return
			}

			playing = true
			if st.Duration > 0 {
				ready = true
			}
			smooth.updateFromMPV(st.TimePos, st.Duration, st.Pause)
			lastMpvUpdateAt = time.Now()

			key := st.FileName
			if key == "" {
				key = st.MediaTitle
			}
			if key != "" && key != currentFileKey {
				lowerKey := strings.ToLower(strings.TrimSpace(key))
				if lowerKey == "mpv" || strings.HasPrefix(lowerKey, "mpv v") {
					playing = false
					ready = false
					currentFileKey = ""
					smooth.reset()
					cache.clear()
					clearTrackingState()
					setPresence(nil)
					traySetIdle()
					continue
				}

				currentFileKey = key
				ready = st.Duration > 0

				md := habari.Parse(key)
				title, ep := pickEpisode(md, fallbackTitleFrom(st))
				ep = applyZeroBasedEpisodeFix(firstNonEmpty(st.Path, st.FileName), key, ep)
				wantYear := wantYearFrom(md, key, st.MediaTitle)
				wantSeason := wantSeasonFrom(key, title, st.MediaTitle)
				seriesKey := seriesKeyForOverride(key, title, st.MediaTitle, wantYear, wantSeason)

				curTrack.mu.Lock()
				curTrack.Active = true
				curTrack.SeriesKey = seriesKey
				curTrack.ParsedTitle = title
				curTrack.MediaTitle = st.MediaTitle
				curTrack.WantYear = wantYear
				curTrack.WantSeason = wantSeason
				curTrack.LastFileKey = key
				curTrack.mu.Unlock()
				setCorrectionMenuActive(seriesKey != "")

				var (
					aname, cover  string
					aid, totalEps int
					aerr          error
				)
				maxWait := 60 * time.Second
				wait := 5 * time.Second
				totalWait := 0 * time.Second
				attempt := 1

				for {
					qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
					if seriesKey != "" {
						if mid := overrides.Get(seriesKey); mid > 0 {
							m, err2 := getAniListMediaByID(qctx, mid, cfg.UserAgent)
							if err2 == nil {
								aname = mediaLiteTitle(m)
								cover = m.CoverImage.Large
								aid = m.ID
								totalEps = m.Episodes
								aerr = nil
							} else if errors.Is(err2, errAniListRateLimited) {
								aerr = err2
							} else {
								aname, cover, aid, totalEps, aerr = findAniList(qctx, title, wantYear, wantSeason, cfg.UserAgent)
							}
						} else {
							aname, cover, aid, totalEps, aerr = findAniList(qctx, title, wantYear, wantSeason, cfg.UserAgent)
						}
					} else {
						aname, cover, aid, totalEps, aerr = findAniList(qctx, title, wantYear, wantSeason, cfg.UserAgent)
					}
					cancel()

					if aerr == nil {
						break
					}
					if !errors.Is(aerr, errAniListRateLimited) {
						break
					}
					if totalWait >= maxWait || ctx.Err() != nil {
						break
					}

					if wait > maxWait-totalWait {
						wait = maxWait - totalWait
					}
					traySetResolving(title, ep, wait, attempt)
					time.Sleep(wait)
					totalWait += wait
					attempt++
				}

				if aerr != nil {
					logAppend("AniList lookup failed: ", aerr.Error())
					aname = title
					cover = ""
					aid = 0
					totalEps = 0
				}

				if strings.TrimSpace(ep) == "" && totalEps == 1 {
					ep = "1"
				}

				cache.mu.Lock()
				cache.lastFile = key
				cache.lastAni = aname
				cache.lastEp = ep
				cache.lastCover = cover
				cache.lastAniID = aid
				cache.lastTotalEps = totalEps
				cache.startEpoch = time.Now().Add(-time.Duration(st.TimePos) * time.Second)
				cache.mu.Unlock()

				curTrack.mu.Lock()
				curTrack.LastAniID = aid
				curTrack.LastTotalEps = totalEps
				curTrack.mu.Unlock()

				traySetWatching(aname, ep, -1, totalEps)
				reported = make(map[string]bool)

				if store.WarnFiller && aid > 0 && strings.TrimSpace(ep) != "" {
					if epNum, err := strconv.Atoi(ep); err == nil && epNum > 0 {
						go func(aid int, aname string, epNum int, filenameTitle, parsedTitle, mediaTitle string) {
							if !markFillerWarned(aid, epNum) {
								return
							}

							titles := []string{
								aname,
								strings.TrimSpace(parsedTitle),
								strings.TrimSpace(filenameTitle),
								strings.TrimSpace(mediaTitle),
							}

							c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
							defer cancel()

							fi, err := getFillerInfo(c, aid, titles)
							if err != nil {
								logAppend("filler: lookup failed: ", err.Error())
								return
							}
							if fi.FillerEps[epNum] {
								msg := fmt.Sprintf("You are watching a filler episode:\n\n%s — episode %d\n\n(From animefillerlist.com)", aname, epNum)
								messageBox("Koushin — Filler episode", msg, mbOK|mbIconInfo)
							}
						}(aid, aname, epNum, key, title, st.MediaTitle)
					}
				}

				pushEstimated()
				broadcastHostState(currentFileKey)
				continue
			}

			if ready && !st.Pause && st.Duration > 0 {
				ratio := st.TimePos / st.Duration
				if ratio >= 0.80 {
					cache.mu.Lock()
					aid := cache.lastAniID
					epStr := cache.lastEp
					totalEps := cache.lastTotalEps
					cache.mu.Unlock()
					if aid > 0 && epStr != "" {
						if epNum, err := strconv.Atoi(epStr); err == nil && epNum > 0 {
							k := fmt.Sprintf("%d:%d", aid, epNum)
							if !reported[k] {
								reported[k] = true
								simulBroadcastSync(simulSyncEvent{AniID: aid, Episode: epNum, TotalEps: totalEps, AtUnixSec: time.Now().Unix()})

								store.mu.RLock()
								tok := store.AccessToken
								uid := store.UserID
								store.mu.RUnlock()
								if strings.TrimSpace(tok) != "" && uid > 0 {
									c, cancel := context.WithTimeout(ctx, 8*time.Second)
									err := syncAniListProgress(c, tok, uid, aid, epNum, totalEps)
									cancel()
									if err != nil {
										logAppend("AniList sync error: ", err.Error())
									}
								}
							}
						}
					}
				}
			}

			pushEstimated()
			broadcastHostState(currentFileKey)

		case <-uiTicker.C:
			if joined() {
				simulApplyJoinStateToUI(cfg, mgr)
				continue
			}
			if playing && cfg.PollInterval > time.Second && (!lastMpvUpdateAt.IsZero()) {
				pushEstimated()
				broadcastHostState(currentFileKey)
			}
		}
	}
}
