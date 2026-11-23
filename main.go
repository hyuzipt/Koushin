package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/5rahim/habari"
	"github.com/natefinch/npipe"
)

// resolveRequest is sent when the user picks a different AniList entry for
// the series that is currently playing, so the loop re-resolves it.
type resolveRequest struct {
	SeriesKey string
	MediaID   int
}

var resolveCh = make(chan resolveRequest, 8)

// presenceCache holds what is currently shown for the playing file.
type presenceCache struct {
	mu           sync.Mutex
	lastAni      string
	lastEp       string
	lastCover    string
	lastAniID    int
	lastTotalEps int
	lastRewatch  bool
}

func (c *presenceCache) clear() {
	c.mu.Lock()
	c.lastAni, c.lastEp, c.lastCover = "", "", ""
	c.lastAniID, c.lastTotalEps = 0, 0
	c.lastRewatch = false
	c.mu.Unlock()
}

func (c *presenceCache) snapshot() (name, ep, cover string, aniID, totalEps int, rewatch bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastAni, c.lastEp, c.lastCover, c.lastAniID, c.lastTotalEps, c.lastRewatch
}

// playbackSnapshot is the playback state shared with Simulwatching peers.
type playbackSnapshot struct {
	Anime      string
	Episode    string
	AniID      int
	TotalEps   int
	CoverURL   string
	Rewatching bool
	Paused     bool
	Pos        float64
	Dur        float64
	Ready      bool
	HostFile   string
	HostSeason int
}

func main() {
	// Single-instance guard: a second copy exits immediately.
	ln, err := net.Listen("tcp", "127.0.0.1:45222")
	if err != nil {
		return
	}
	defer ln.Close()

	firstRun := !fileExists(store.file())
	cfg := loadConfig()
	store.Load()
	loadZeroBasedSignatures()

	if firstRun {
		store.mu.Lock()
		store.RunOnStartup = true
		store.mu.Unlock()
		store.Save()
	}
	store.mu.RLock()
	runOnStartup := store.RunOnStartup
	store.mu.RUnlock()
	if err := setWindowsRunOnStartup(runOnStartup); err != nil {
		logAppend("startup: failed to apply auto-start setting: ", err.Error())
	}
	if err := ensureStartMenuShortcut(); err != nil {
		logAppend("startmenu: failed to ensure shortcut: ", err.Error())
	}

	overrides.Load()
	runWithTray(func(ctx context.Context) { run(ctx, cfg) }, nil)
}

func run(ctx context.Context, cfg Config) {
	mgr := &discordManager{}
	defer mgr.close(cfg.DiscordAppID)
	go runSuwayomiRPC(ctx, cfg)

	if !discordRPCEnabled() {
		discordApplyActivity(cfg.DiscordAppID, mgr, nil)
	}

	for ctx.Err() == nil {
		if simulJoined() {
			simulApplyJoinStateToUI(cfg, mgr)
			sleepCtx(ctx, 500*time.Millisecond)
			continue
		}
		conn, err := npipe.DialTimeout(cfg.MpvPipe, 2*time.Second)
		if err != nil {
			discordApplyActivity(cfg.DiscordAppID, mgr, nil)
			traySetIdle()
			sleepCtx(ctx, 1500*time.Millisecond)
			continue
		}

		runLoop(ctx, cfg, newMpvClient(conn), mgr)
		traySetIdle()
		sleepCtx(ctx, time.Second)
	}
}

// sleepCtx sleeps for d or until ctx is cancelled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// resolveAniList finds the AniList entry for a file, honouring a manual
// override for the series. Rate-limit errors are returned as-is so the
// caller can back off and retry.
func resolveAniList(ctx context.Context, seriesKey, title string, wantYear, wantSeason int, ua string) (name, cover string, id, totalEps int, err error) {
	if mid := overrides.Get(seriesKey); mid > 0 {
		m, err := getAniListMediaByID(ctx, mid, ua)
		if err == nil {
			return mediaLiteTitle(m), m.CoverImage.Large, m.ID, m.Episodes, nil
		}
		if errors.Is(err, errAniListRateLimited) {
			return "", "", 0, 0, err
		}
	}
	return findAniList(ctx, title, wantYear, wantSeason, ua)
}

// warnIfFiller shows a one-time message box if the episode is listed as
// filler on animefillerlist.com.
func warnIfFiller(aid int, aname string, epNum int, titles []string) {
	if !markFillerWarned(aid, epNum) {
		return
	}
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fi, err := getFillerInfo(c, aid, titles)
	if err != nil {
		logAppend("filler: lookup failed: ", err.Error())
		return
	}
	if fi.FillerEps[epNum] {
		msg := fmt.Sprintf("You are watching a filler episode:\n\n%s - episode %d\n\n(From animefillerlist.com)", aname, epNum)
		messageBox("Koushin: Filler episode", msg, mbOK|mbIconInfo)
	}
}

func runLoop(ctx context.Context, cfg Config, mpv *mpvClient, mgr *discordManager) {
	defer mpv.Close()

	cache := &presenceCache{}
	smooth := &progSmooth{}
	mpvTicker := time.NewTicker(cfg.PollInterval)
	uiTicker := time.NewTicker(time.Second)
	defer mpvTicker.Stop()
	defer uiTicker.Stop()

	var (
		currentFileKey  string
		lastMpvUpdateAt time.Time
		playing         bool
		ready           bool
		reported        = make(map[string]bool) // "aniID:episode" already synced
	)
	clearTrackingState()

	setPresence := func(activity map[string]any) {
		discordApplyActivity(cfg.DiscordAppID, mgr, activity)
	}

	stopPlayback := func() {
		playing = false
		ready = false
		currentFileKey = ""
		smooth.reset()
		cache.clear()
		clearTrackingState()
		setPresence(nil)
		traySetIdle()
	}

	// position returns the smoothed playback position. Until mpv reports a
	// duration the position is pinned to 0.
	position := func() (pos, dur float64, paused bool) {
		pos, dur, paused = smooth.estimate()
		if !ready {
			pos = 0
		}
		return
	}

	publishHostState := func() {
		aname, ep, cover, aniID, totalEps, rewatch := cache.snapshot()
		pos, dur, paused := position()
		curTrack.mu.RLock()
		season := curTrack.WantSeason
		curTrack.mu.RUnlock()
		simulPublish(playbackSnapshot{
			Anime:      strings.TrimSpace(aname),
			Episode:    strings.TrimSpace(ep),
			AniID:      aniID,
			TotalEps:   totalEps,
			CoverURL:   strings.TrimSpace(cover),
			Rewatching: rewatch,
			Paused:     paused,
			Pos:        pos,
			Dur:        dur,
			Ready:      ready && playing,
			HostFile:   strings.TrimSpace(currentFileKey),
			HostSeason: season,
		})
	}

	pushEstimated := func() {
		if !playing {
			return
		}
		aname, ep, cover, aniID, totalEps, rewatch := cache.snapshot()
		pos, dur, paused := position()

		episodeLabel := ""
		if strings.TrimSpace(ep) != "" {
			if totalEps > 0 {
				episodeLabel = fmt.Sprintf("%s/%d", ep, totalEps)
			} else {
				episodeLabel = ep + "/??"
			}
		}

		act := buildActivity(aname, episodeLabel, cover, cfg.SmallImage, anilistURL(aniID), pos, dur, paused, rewatch)
		if !ready {
			delete(act, "timestamps")
		}
		setPresence(act)

		percent := -1
		if dur > 0 {
			percent = min(max(int(pos/dur*100+0.5), 0), 100)
		}
		traySetWatching(aname, ep, percent, totalEps)
	}

	// onNewFile parses the file name, resolves it on AniList and refreshes
	// everything that depends on the current show.
	onNewFile := func(key string, st mpvState) {
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

		// Resolve on AniList, backing off for up to a minute while rate limited.
		var (
			aname, cover  string
			aid, totalEps int
			aerr          error
		)
		const maxWait = 60 * time.Second
		wait, totalWait := 5*time.Second, time.Duration(0)
		for attempt := 1; ; attempt++ {
			qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			aname, cover, aid, totalEps, aerr = resolveAniList(qctx, seriesKey, title, wantYear, wantSeason, cfg.UserAgent)
			cancel()

			if aerr == nil || !errors.Is(aerr, errAniListRateLimited) || totalWait >= maxWait || ctx.Err() != nil {
				break
			}
			wait = min(wait, maxWait-totalWait)
			traySetResolving(title, ep, wait, attempt)
			sleepCtx(ctx, wait)
			totalWait += wait
		}

		if aerr != nil {
			logAppend("AniList lookup failed: ", aerr.Error())
			aname, cover, aid, totalEps = title, "", 0, 0
		}
		if strings.TrimSpace(ep) == "" && totalEps == 1 {
			ep = "1"
		}

		rewatch := anilistIsRewatching(ctx, aid)
		cache.mu.Lock()
		cache.lastAni = aname
		cache.lastEp = ep
		cache.lastCover = cover
		cache.lastAniID = aid
		cache.lastTotalEps = totalEps
		cache.lastRewatch = rewatch
		cache.mu.Unlock()

		curTrack.mu.Lock()
		curTrack.LastAniID = aid
		curTrack.LastTotalEps = totalEps
		curTrack.LastTitle = aname
		curTrack.LastCover = cover
		curTrack.mu.Unlock()

		traySetWatching(aname, ep, -1, totalEps)
		reported = make(map[string]bool)

		if store.warnFiller() && aid > 0 {
			if epNum, err := strconv.Atoi(strings.TrimSpace(ep)); err == nil && epNum > 0 {
				titles := []string{aname, strings.TrimSpace(title), strings.TrimSpace(key), strings.TrimSpace(st.MediaTitle)}
				go warnIfFiller(aid, aname, epNum, titles)
			}
		}
	}

	// syncProgressIfDue updates AniList once an episode is ~80% watched.
	syncProgressIfDue := func(st mpvState) {
		if !ready || st.Pause || st.Duration <= 0 || st.TimePos/st.Duration < 0.80 {
			return
		}
		_, epStr, _, aid, totalEps, _ := cache.snapshot()
		epNum, err := strconv.Atoi(epStr)
		if aid <= 0 || err != nil || epNum <= 0 {
			return
		}
		k := fmt.Sprintf("%d:%d", aid, epNum)
		if reported[k] {
			return
		}
		reported[k] = true
		simulPublishSync(aid, epNum, totalEps)

		if !anilistTrackingEnabled() {
			return
		}
		store.mu.RLock()
		tok, uid := store.AccessToken, store.UserID
		store.mu.RUnlock()
		c, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		if err := syncAniListProgress(c, tok, uid, aid, epNum, totalEps); err != nil {
			logAppend("AniList sync error: ", err.Error())
		}
	}

	for {
		select {
		case <-ctx.Done():
			return

		case rr := <-resolveCh:
			curTrack.mu.RLock()
			active, seriesKey := curTrack.Active, curTrack.SeriesKey
			curTrack.mu.RUnlock()
			if active && seriesKey != "" && rr.SeriesKey == seriesKey {
				currentFileKey = "" // forces a re-resolve on the next tick
			}

		case <-mpvTicker.C:
			if simulJoined() {
				simulApplyJoinStateToUI(cfg, mgr)
				continue
			}
			st, err := mpv.state()
			if err != nil {
				simulPublishIdle()
				stopPlayback()
				switch {
				case errors.Is(err, errMpvNoFile) && mpv.alive():
					continue // mpv is open but idle
				case errors.Is(err, errMpvNoFile), isPipeGone(err):
					logAppend("mpv: connection closed, reconnecting")
				default:
					logAppend("mpv: ", err.Error())
				}
				return
			}

			playing = true
			if st.Duration > 0 {
				ready = true
			}
			smooth.updateFromMPV(st.TimePos, st.Duration, st.Pause)
			lastMpvUpdateAt = time.Now()

			key := firstNonEmpty(st.FileName, st.MediaTitle)
			if key != "" && key != currentFileKey {
				// mpv's idle screen reports its own name as the title.
				lowerKey := strings.ToLower(strings.TrimSpace(key))
				if lowerKey == "mpv" || strings.HasPrefix(lowerKey, "mpv v") {
					stopPlayback()
					continue
				}
				currentFileKey = key
				ready = st.Duration > 0
				onNewFile(key, st)
				pushEstimated()
				publishHostState()
				continue
			}

			syncProgressIfDue(st)
			pushEstimated()
			publishHostState()

		case <-uiTicker.C:
			if simulJoined() {
				simulApplyJoinStateToUI(cfg, mgr)
				continue
			}
			// With slow mpv polling, keep the presence ticking between polls.
			if playing && cfg.PollInterval > time.Second && !lastMpvUpdateAt.IsZero() {
				pushEstimated()
				publishHostState()
			}
		}
	}
}
