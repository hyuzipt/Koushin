package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getlantern/systray"
)

//go:embed koushin.ico
var iconICO []byte

const trayTitleBase = "Koushin"

var (
	menuDiscordRPC    *systray.MenuItem
	menuToggleProfile *systray.MenuItem
	menuAniList       *systray.MenuItem // "Sign in to AniList…" / "Sign out of @user"
	menuCorrectAnime  *systray.MenuItem
	menuAniListTrack  *systray.MenuItem
	menuFillerWarn    *systray.MenuItem
	menuRunOnStartup  *systray.MenuItem
	menuCheckUpdate   *systray.MenuItem
	menuQuit          *systray.MenuItem

	loginCancelMu   sync.Mutex
	loginCancelFunc context.CancelFunc // non-nil while an AniList login is in progress
	loginGen        int                // identifies the current login attempt
)

// trackState describes the file currently being tracked. It is read by the
// "Select correct anime…" flow.
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
	LastTitle    string // resolved AniList title, shown in the selector
	LastCover    string
}

var curTrack trackState

func setTrayIcon() {
	if len(iconICO) > 0 {
		systray.SetIcon(iconICO)
		return
	}
	if b, err := os.ReadFile("koushin.ico"); err == nil {
		systray.SetIcon(b)
	}
}

func setChecked(item *systray.MenuItem, on bool) {
	if item == nil {
		return
	}
	if on {
		item.Check()
	} else {
		item.Uncheck()
	}
}

func setEnabled(item *systray.MenuItem, on bool) {
	if item == nil {
		return
	}
	if on {
		item.Enable()
	} else {
		item.Disable()
	}
}

func setCorrectionMenuActive(active bool) {
	setEnabled(menuCorrectAnime, active)
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
	curTrack.LastTitle = ""
	curTrack.LastCover = ""
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
	systray.SetTooltip(name + epPart + pctPart)
}

func traySetResolving(name, ep string, wait time.Duration, attempt int) {
	base := firstNonEmpty(strings.TrimSpace(name), trayTitleBase)
	epPart := ""
	if ep = strings.TrimSpace(ep); ep != "" {
		epPart = " · Ep " + ep + "/??"
	}
	systray.SetTooltip(fmt.Sprintf("%s%s · AniList rate limited, retrying in %ds (try %d)", base, epPart, int(wait.Seconds()), attempt))
}

func loginInProgress() bool {
	loginCancelMu.Lock()
	defer loginCancelMu.Unlock()
	return loginCancelFunc != nil
}

// refreshAuthMenu syncs every menu item with the stored settings.
func refreshAuthMenu() {
	store.mu.RLock()
	signedIn := store.AccessToken != ""
	username := store.Username
	showAni := store.ShowAniProfile
	rpcOn := store.DiscordRPC
	trackOn := store.AniListTracking
	warnFiller := store.WarnFiller
	store.mu.RUnlock()

	setChecked(menuDiscordRPC, rpcOn)
	setChecked(menuFillerWarn, warnFiller)
	setChecked(menuAniListTrack, trackOn)
	setEnabled(menuAniListTrack, signedIn)
	setChecked(menuToggleProfile, signedIn && showAni)
	setEnabled(menuToggleProfile, signedIn && rpcOn)

	// While logging in, the item reads "Cancel login…"; leave it alone.
	if menuAniList == nil || loginInProgress() {
		return
	}
	if signedIn {
		title := "Sign out of AniList"
		if username != "" {
			title = "Sign out of @" + username
		}
		menuAniList.SetTitle(title)
		menuAniList.SetTooltip("Forget the saved AniList token on this device")
	} else {
		menuAniList.SetTitle("Sign in to AniList…")
		menuAniList.SetTooltip("Authenticate this device with AniList")
	}
	menuAniList.Enable()
}

// onAniListClicked signs in, signs out, or cancels a pending login.
func onAniListClicked(ctx context.Context) {
	loginCancelMu.Lock()
	if loginCancelFunc != nil {
		loginCancelFunc()
		loginCancelFunc = nil
		loginCancelMu.Unlock()
		refreshAuthMenu()
		return
	}
	loginCancelMu.Unlock()

	store.mu.RLock()
	signedIn := store.AccessToken != ""
	store.mu.RUnlock()
	if signedIn {
		store.ClearAniList()
		refreshAuthMenu()
		return
	}

	loginCtx, loginCancel := context.WithCancel(ctx)
	loginCancelMu.Lock()
	loginCancelFunc = loginCancel
	loginGen++
	gen := loginGen
	loginCancelMu.Unlock()
	menuAniList.SetTitle("Cancel login…")

	go func() {
		err := oauthLogin(loginCtx)
		loginCancel()

		loginCancelMu.Lock()
		if loginGen == gen { // a newer attempt may have started after a cancel
			loginCancelFunc = nil
		}
		loginCancelMu.Unlock()

		switch {
		case errors.Is(err, context.Canceled):
			logAppend("AniList login cancelled by user")
		case err != nil:
			logAppend("AniList login failed: ", err.Error())
		}
		store.Load()
		refreshAuthMenu()
	}()
}

func onCorrectAnimeClicked() {
	curTrack.mu.RLock()
	active := curTrack.Active
	seriesKey := curTrack.SeriesKey
	query := cleanTitleForSearch(firstNonEmpty(curTrack.ParsedTitle, curTrack.MediaTitle))
	file := curTrack.LastFileKey
	sel := selectorContext{
		Query:        query,
		File:         file,
		CurrentID:    curTrack.LastAniID,
		CurrentTitle: curTrack.LastTitle,
		CurrentCover: curTrack.LastCover,
	}
	curTrack.mu.RUnlock()
	if !active || seriesKey == "" {
		return
	}

	go func() {
		err := startAniListSelector(sel, loadConfig().UserAgent, func(id int) {
			overrides.Set(seriesKey, id)
			select {
			case resolveCh <- resolveRequest{SeriesKey: seriesKey, MediaID: id}:
			default:
			}
		})
		if err != nil {
			logAppend("selector: ", err.Error())
		}
	}()
}

func onReadyTray(ctx context.Context, cancel context.CancelFunc) {
	setTrayIcon()
	systray.SetTooltip(trayTitleBase)

	store.mu.RLock()
	rpcOn, showAni, trackOn := store.DiscordRPC, store.ShowAniProfile, store.AniListTracking
	warnFiller, runOnStartup := store.WarnFiller, store.RunOnStartup
	store.mu.RUnlock()

	menuDiscordRPC = systray.AddMenuItemCheckbox("Enable Discord Rich Presence",
		"Toggle Discord Rich Presence updates (tray + AniList still work)", rpcOn)
	menuToggleProfile = systray.AddMenuItemCheckbox("Show AniList profile in Discord RPC",
		"Toggle AniList profile small icon", showAni)
	systray.AddSeparator()

	menuAniList = systray.AddMenuItem("Sign in to AniList…", "Authenticate this device with AniList")
	menuCorrectAnime = systray.AddMenuItem("Select correct anime…", "Manually select the AniList anime for the current file")
	menuCorrectAnime.Disable()
	menuAniListTrack = systray.AddMenuItemCheckbox("Enable AniList tracking",
		"Toggle whether Koushin updates your AniList progress while watching", trackOn)
	menuFillerWarn = systray.AddMenuItemCheckbox("Warn for filler episodes",
		"Show a warning when watching filler episodes (when available)", warnFiller)
	systray.AddSeparator()

	addSimulMenu(ctx) // no-op unless built with -tags simul

	menuRunOnStartup = systray.AddMenuItemCheckbox("Run on Windows startup",
		"Automatically start Koushin when you sign in to Windows", runOnStartup)
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
				if !store.toggle(&store.DiscordRPC) {
					// Clear the activity right away instead of waiting for the next tick.
					go func() {
						appID := loadConfig().DiscordAppID
						m := &discordManager{}
						m.setActivity(appID, nil)
						m.close(appID)
					}()
				}
				refreshAuthMenu()

			case <-menuToggleProfile.ClickedCh:
				store.mu.RLock()
				allowed := store.AccessToken != "" && store.DiscordRPC
				store.mu.RUnlock()
				if allowed {
					store.toggle(&store.ShowAniProfile)
				}
				refreshAuthMenu()

			case <-menuAniList.ClickedCh:
				onAniListClicked(ctx)

			case <-menuCorrectAnime.ClickedCh:
				onCorrectAnimeClicked()

			case <-menuAniListTrack.ClickedCh:
				store.toggle(&store.AniListTracking)
				refreshAuthMenu()

			case <-menuFillerWarn.ClickedCh:
				store.toggle(&store.WarnFiller)
				refreshAuthMenu()

			case <-menuRunOnStartup.ClickedCh:
				on := store.toggle(&store.RunOnStartup)
				setChecked(menuRunOnStartup, on)
				if err := setWindowsRunOnStartup(on); err != nil {
					logAppend("startup: failed to update HKCU Run entry: ", err.Error())
					messageBox("Koushin: Startup setting failed", "Could not update Windows startup setting:\n\n"+err.Error(), mbOK|mbIconError)
				}

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

	// Check for updates shortly after startup.
	go func() {
		sleepCtx(ctx, 5*time.Second)
		if ctx.Err() != nil {
			return
		}
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
