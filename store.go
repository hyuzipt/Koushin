package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// authStore holds the AniList session and every user setting. It is
// persisted to auth.json in the app data folder.
type authStore struct {
	mu   sync.RWMutex
	Path string

	AccessToken     string
	Username        string
	UserID          int
	ShowAniProfile  bool
	AniListTracking bool

	DiscordRPC   bool
	WarnFiller   bool
	RunOnStartup bool

	// Simulwatching settings. The feature is currently disabled (see
	// simul.go), but the values are kept so they survive a round trip.
	SimulPort     int
	SimulAutoUPnP bool
	SimulJoinSync bool
}

// authFile is the on-disk layout of auth.json. Pointer fields default to
// true when they are missing from older files.
type authFile struct {
	AccessToken     string `json:"access_token"`
	Username        string `json:"username"`
	UserID          int    `json:"user_id"`
	ShowAniProfile  bool   `json:"show_ani_profile"`
	AniListTracking *bool  `json:"anilist_tracking"`

	DiscordRPC    *bool `json:"discord_rpc"`
	WarnFiller    bool  `json:"warn_filler"`
	RunOnStartup  bool  `json:"run_on_startup"`
	SimulPort     int   `json:"simul_port"`
	SimulAutoUPnP *bool `json:"simul_auto_upnp"`
	SimulJoinSync bool  `json:"simul_join_sync"`
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
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
	var f authFile
	if json.Unmarshal(b, &f) != nil {
		return
	}
	a.AccessToken = f.AccessToken
	a.Username = f.Username
	a.UserID = f.UserID
	a.ShowAniProfile = f.ShowAniProfile
	a.AniListTracking = boolOr(f.AniListTracking, true)

	a.DiscordRPC = boolOr(f.DiscordRPC, true)
	a.WarnFiller = f.WarnFiller
	a.RunOnStartup = f.RunOnStartup
	a.SimulPort = f.SimulPort
	a.SimulAutoUPnP = boolOr(f.SimulAutoUPnP, true)
	a.SimulJoinSync = f.SimulJoinSync
}

func (a *authStore) Save() {
	a.mu.RLock()
	f := authFile{
		AccessToken:     a.AccessToken,
		Username:        a.Username,
		UserID:          a.UserID,
		ShowAniProfile:  a.ShowAniProfile,
		AniListTracking: &a.AniListTracking,

		DiscordRPC:    &a.DiscordRPC,
		WarnFiller:    a.WarnFiller,
		RunOnStartup:  a.RunOnStartup,
		SimulPort:     a.SimulPort,
		SimulAutoUPnP: &a.SimulAutoUPnP,
		SimulJoinSync: a.SimulJoinSync,
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	a.mu.RUnlock()
	_ = os.WriteFile(a.file(), b, 0600)
}

// ClearAniList signs out of AniList. Other settings are left untouched.
func (a *authStore) ClearAniList() {
	a.mu.Lock()
	a.AccessToken = ""
	a.Username = ""
	a.UserID = 0
	a.ShowAniProfile = false
	a.mu.Unlock()
	a.Save()
}

// toggle flips a boolean setting under the lock, saves, and returns the new value.
func (a *authStore) toggle(field *bool) bool {
	a.mu.Lock()
	*field = !*field
	on := *field
	a.mu.Unlock()
	a.Save()
	return on
}

func (a *authStore) warnFiller() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.WarnFiller
}

var store = &authStore{Path: appDataDir(), WarnFiller: true, DiscordRPC: true, AniListTracking: true}

// overrideStore remembers manual "Select correct anime" picks, keyed by series.
type overrideStore struct {
	mu   sync.RWMutex
	Path string
	ByK  map[string]int
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
	if json.Unmarshal(b, &tmp) == nil && tmp.ByK != nil {
		o.ByK = tmp.ByK
	}
}

func (o *overrideStore) Save() {
	o.mu.RLock()
	b, _ := json.MarshalIndent(struct {
		ByK map[string]int `json:"by_key"`
	}{ByK: o.ByK}, "", "  ")
	o.mu.RUnlock()
	_ = os.WriteFile(o.file(), b, 0600)
}

func (o *overrideStore) Get(key string) int {
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
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
