package main

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const appVersion = "0.3.0"

const (
	githubOwner = "hyuzipt"
	githubRepo  = "Koushin"
)

const (
	anilistClientID      = "31833"
	discordAppID         = "1434412611411120198"
	suwayomiDiscordAppID = "1485726894795133021"

	localLoginAddr = "127.0.0.1:45124"

	anilistGQLURL = "https://graphql.anilist.co"
)

type Config struct {
	DiscordAppID         string
	SuwayomiDiscordAppID string
	MpvPipe              string
	PollInterval         time.Duration
	UserAgent            string
	SmallImage           string
}

// loadConfig builds the runtime config from defaults and optional
// environment overrides (MPV_PIPE, POLL_MS, HTTP_USER_AGENT, SUWAYOMI_DISCORD_APP_ID).
func loadConfig() Config {
	cfg := Config{
		DiscordAppID:         discordAppID,
		SuwayomiDiscordAppID: suwayomiDiscordAppID,
		MpvPipe:              `\\.\pipe\mpv-pipe`,
		PollInterval:         250 * time.Millisecond,
		UserAgent:            "koushin/" + appVersion + " (+https://github.com/" + githubOwner + "/" + githubRepo + ")",
		SmallImage:           "anilist",
	}
	if v := strings.TrimSpace(os.Getenv("SUWAYOMI_DISCORD_APP_ID")); v != "" {
		cfg.SuwayomiDiscordAppID = v
	}
	if v := os.Getenv("MPV_PIPE"); v != "" {
		cfg.MpvPipe = v
	}
	if v := os.Getenv("POLL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 200 {
			cfg.PollInterval = time.Duration(ms) * time.Millisecond
		}
	}
	if v := os.Getenv("HTTP_USER_AGENT"); v != "" {
		cfg.UserAgent = v
	}
	return cfg
}

var httpClient = &http.Client{Timeout: 10 * time.Second}
