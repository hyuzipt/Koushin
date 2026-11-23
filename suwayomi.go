package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type suwayomiChapterNode struct {
	ID            int     `json:"id"`
	Name          string  `json:"name"`
	ChapterNumber float64 `json:"chapterNumber"`
	LastReadAt    string  `json:"lastReadAt"`
	Manga         struct {
		ID     int    `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
	} `json:"manga"`
}

type anilistMangaLite struct {
	ID    int `json:"id"`
	Title struct {
		English string `json:"english"`
		Romaji  string `json:"romaji"`
	} `json:"title"`
	CoverImage struct {
		ExtraLarge string `json:"extraLarge"`
		Large      string `json:"large"`
	} `json:"coverImage"`
	Chapters int    `json:"chapters"`
	Status   string `json:"status"`
}

func suwayomiGraphQLURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "http://localhost:4567"
	}
	base = strings.TrimRight(base, "/")
	return base + "/api/graphql"
}

func suwayomiAlive(ctx context.Context, base string) bool {
	url := suwayomiGraphQLURL(base)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

func fetchSuwayomiRecentChapter(ctx context.Context, base string) (*suwayomiChapterNode, error) {
	const q = `
query RecentChapter {
  chapters(
    orderBy: LAST_READ_AT
    orderByType: DESC
    first: 1
    filter: { lastReadAt: { greaterThan: "0" } }
  ) {
    nodes {
      id
      name
      chapterNumber
      lastReadAt
      manga { id title status }
    }
  }
}`
	url := suwayomiGraphQLURL(base)
	body := map[string]any{"query": q}
	bs, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bs))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("suwayomi http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data struct {
			Chapters struct {
				Nodes []suwayomiChapterNode `json:"nodes"`
			} `json:"chapters"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data.Chapters.Nodes) == 0 {
		return nil, nil
	}
	return &out.Data.Chapters.Nodes[0], nil
}

func fetchAniListManga(ctx context.Context, title string) (*anilistMangaLite, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, errors.New("empty title")
	}
	const q = `
query ($search: String) {
  Media(search: $search, type: MANGA, sort: SEARCH_MATCH) {
    id
    title { english romaji }
    coverImage { extraLarge large }
    chapters
    status
  }
}`
	var out struct {
		Data struct {
			Media *anilistMangaLite `json:"Media"`
		} `json:"data"`
	}
	if err := anilistGraphQL(ctx, "", "", q, map[string]any{"search": title}, &out); err != nil {
		return nil, err
	}
	return out.Data.Media, nil
}

func anilistMangaURL(id int) string {
	if id <= 0 {
		return ""
	}
	return fmt.Sprintf("https://anilist.co/manga/%d", id)
}

func bestMangaTitle(al *anilistMangaLite, fallback string) string {
	if al != nil {
		if t := strings.TrimSpace(al.Title.English); t != "" {
			return t
		}
		if t := strings.TrimSpace(al.Title.Romaji); t != "" {
			return t
		}
	}
	return strings.TrimSpace(fallback)
}

func mangaCoverURL(al *anilistMangaLite) string {
	if al == nil {
		return ""
	}
	if s := strings.TrimSpace(al.CoverImage.ExtraLarge); s != "" {
		return s
	}
	if s := strings.TrimSpace(al.CoverImage.Large); s != "" {
		return s
	}
	return ""
}

func suwayomiChapterState(ch *suwayomiChapterNode, al *anilistMangaLite) string {
	if ch == nil {
		return ""
	}
	n := ch.ChapterNumber
	label := "Reading: Ch. "
	if math.Abs(n-math.Round(n)) < 1e-9 {
		label += strconv.Itoa(int(math.Round(n)))
	} else {
		label += strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", n), "0"), ".")
	}

	total := 0
	if al != nil {
		total = al.Chapters
	}
	st := strings.TrimSpace(ch.Manga.Status)
	if strings.EqualFold(st, "COMPLETED") && total > 0 {
		return fmt.Sprintf("%s / %d chapters", label, total)
	}
	return label
}

func buildSuwayomiActivity(title, state, coverURL, smallKey, aniURL string, startedAt time.Time) map[string]any {
	title = strings.TrimSpace(title)
	state = strings.TrimSpace(state)
	if state == "" {
		return nil
	}

	name := "Suwayomi"
	details := title
	if details == "" {
		details = name
	}
	stateLine := state
	img := strings.TrimSpace(coverURL)
	if img == "" {
		img = "suwayomi"
	}
	assets := map[string]any{
		"large_image": img,
		"large_text":  firstNonEmpty(title, details),
	}
	if strings.TrimSpace(aniURL) != "" {
		assets["large_url"] = strings.TrimSpace(aniURL)
	}

	store.mu.RLock()
	showProfile := store.ShowAniProfile && strings.TrimSpace(store.AccessToken) != ""
	username := store.Username
	store.mu.RUnlock()
	if showProfile && strings.TrimSpace(smallKey) != "" {
		assets["small_image"] = strings.TrimSpace(smallKey)
		name := strings.TrimSpace(username)
		if name == "" {
			name = "AniList user"
		}
		assets["small_text"] = name + " on AniList"
	} else {
		assets["small_image"] = "suwayomi"
		assets["small_text"] = "Suwayomi"
	}

	act := map[string]any{
		"name":    name,
		"details": details,
		"state":   stateLine,
		"type":    3,
		"assets":  assets,
	}
	if !startedAt.IsZero() {
		act["timestamps"] = map[string]any{"start": startedAt.Unix()}
	}
	return act
}

func runSuwayomiRPC(ctx context.Context, cfg Config) {
	defer logRecoveredPanic("runSuwayomiRPC")

	base := strings.TrimSpace(os.Getenv("SUWAYOMI_URL"))
	if base == "" {
		base = "http://localhost:4567"
	}

	poll := 5 * time.Second
	if v := strings.TrimSpace(os.Getenv("SUWAYOMI_POLL_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 60 {
			poll = time.Duration(n) * time.Second
		}
	}

	idleTimeout := 120 * time.Second
	if v := strings.TrimSpace(os.Getenv("SUWAYOMI_READING_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 10 && n <= 600 {
			idleTimeout = time.Duration(n) * time.Second
		}
	}

	appID := strings.TrimSpace(cfg.SuwayomiDiscordAppID)
	if v := strings.TrimSpace(os.Getenv("SUWAYOMI_DISCORD_APP_ID")); v != "" {
		appID = v
	}
	if appID == "" {
		appID = suwayomiDiscordAppID
	}

	mgr := &discordManager{}
	defer mgr.close(appID)

	cache := make(map[int]*anilistMangaLite)
	var lastMangaID int
	var startedAt time.Time

	lastSeenLastReadAt := ""
	var lastChangedAt time.Time

	t := time.NewTicker(poll)
	defer t.Stop()

	clear := func() {
		mgr.setActivity(appID, nil)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		if !discordRPCEnabled() {
			mgr.close(appID)
			continue
		}

		qctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		ch, err := fetchSuwayomiRecentChapter(qctx, base)
		cancel()
		if err != nil {
			q2, c2 := context.WithTimeout(ctx, 2*time.Second)
			alive := suwayomiAlive(q2, base)
			c2()
			if !alive {
				clear()
			}
			continue
		}
		if ch == nil {
			clear()
			continue
		}

		if strings.TrimSpace(ch.LastReadAt) != "" {
			if ch.LastReadAt != lastSeenLastReadAt {
				lastSeenLastReadAt = ch.LastReadAt
				lastChangedAt = time.Now()
			}
		}
		active := !lastChangedAt.IsZero() && time.Since(lastChangedAt) < idleTimeout
		if !active {
			clear()
			continue
		}

		mangaID := ch.Manga.ID
		rawTitle := strings.TrimSpace(ch.Manga.Title)
		al := cache[mangaID]
		if al == nil && rawTitle != "" {
			c3, cancel3 := context.WithTimeout(ctx, 8*time.Second)
			m, err := fetchAniListManga(c3, rawTitle)
			cancel3()
			if err == nil {
				al = m
				cache[mangaID] = m
			}
		}

		title := bestMangaTitle(al, rawTitle)
		state := suwayomiChapterState(ch, al)
		cover := mangaCoverURL(al)
		aniURL := ""
		if al != nil {
			aniURL = anilistMangaURL(al.ID)
		}
		if mangaID != 0 && mangaID != lastMangaID {
			lastMangaID = mangaID
			startedAt = time.Now()
		}
		act := buildSuwayomiActivity(title, state, cover, cfg.SmallImage, aniURL, startedAt)
		mgr.setActivity(appID, act)
	}
}
