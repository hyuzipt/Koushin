package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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

// isNewerVersion reports whether latest is a higher version than current,
// comparing dot-separated numbers ("0.10.0" > "0.9.3"). Non-numeric parts
// (like "-beta") are ignored. If either version can't be parsed, any
// difference counts as newer, matching the previous behaviour.
func isNewerVersion(current, latest string) bool {
	parse := func(v string) ([]int, bool) {
		v = normalizeVersion(v)
		if i := strings.IndexAny(v, "-+ "); i >= 0 {
			v = v[:i]
		}
		var out []int
		for _, p := range strings.Split(v, ".") {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil, false
			}
			out = append(out, n)
		}
		return out, len(out) > 0
	}
	c, okC := parse(current)
	l, okL := parse(latest)
	if !okC || !okL {
		return normalizeVersion(current) != normalizeVersion(latest)
	}
	for i := 0; i < max(len(c), len(l)); i++ {
		var a, b int
		if i < len(c) {
			a = c[i]
		}
		if i < len(l) {
			b = l[i]
		}
		if a != b {
			return b > a
		}
	}
	return false
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

	cmd := hiddenCommand("cmd", "/c", batPath, exePath, newPath)
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
			messageBox("Koushin: Update check failed",
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

	res := messageBox("Koushin: Update available",
		fmt.Sprintf("A new version of Koushin is available.\n\nCurrent: %s\nLatest: %s\n\nUpdate now?", cur, latest),
		mbYesNo|mbIconQuestion)
	if res != idYes {
		return
	}

	uCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := performSelfUpdate(uCtx, tag, url); err != nil {
		messageBox("Koushin: Update failed",
			"Failed to update:\n"+err.Error(),
			mbOK|mbIconError)
		return
	}

	messageBox("Koushin: Updating",
		"Koushin will now close and restart with the updated version.",
		mbOK|mbIconInfo)

	os.Exit(0)
}
