package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/natefinch/npipe"
)

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

const fallbackLargeImageKey = "koushin"

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

func buildActivity(title, episode, coverURL, smallKey, aniURL string, cur, dur float64, paused bool, rewatching bool) map[string]any {
	details := title
	epText := "Episode:"
	if strings.TrimSpace(episode) != "" {
		epText = "Episode " + episode
	}
	parts := make([]string, 0, 3)
	if paused {
		parts = append(parts, "Paused")
	}
	if rewatching {
		parts = append(parts, "Rewatching")
	}
	parts = append(parts, epText)
	state := strings.Join(parts, " - ")

	img := coverURL
	if strings.TrimSpace(img) == "" {
		img = fallbackLargeImageKey
	}

	assets := map[string]any{
		"large_image": img,
		"large_text":  title,
	}
	if aniURL != "" {
		assets["large_url"] = aniURL
	}

	store.mu.RLock()
	showProfile := store.ShowAniProfile && strings.TrimSpace(store.AccessToken) != ""
	username := store.Username
	store.mu.RUnlock()

	if smallKey != "" && showProfile {
		assets["small_image"] = smallKey
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
