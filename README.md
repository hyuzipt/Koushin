<div align="center">

<img src="assets/koushin.png" width="96" alt="Koushin">

# Koushin

**Discord Rich Presence and AniList / MyAnimeList sync for anime you watch in mpv.**

A small Windows tray app. You download it, run it, and forget it's there.

[![Release](https://img.shields.io/github/v/release/hyuzipt/Koushin?style=flat-square&color=c0392b&label=release)](https://github.com/hyuzipt/Koushin/releases/latest)
[![Downloads](https://img.shields.io/github/downloads/hyuzipt/Koushin/total?style=flat-square&color=555)](https://github.com/hyuzipt/Koushin/releases)
![Windows](https://img.shields.io/badge/windows-only-555?style=flat-square&logo=windows)
![Go](https://img.shields.io/badge/go-1.23-555?style=flat-square&logo=go&logoColor=white)

<img src="assets/discord-rpc.png" width="380" alt="Koushin on Discord">

</div>

## Features

- **Rich Presence**: title, episode, cover art and a live progress bar that keeps up with seeking and pausing
- **List sync**: updates your AniList and/or MyAnimeList progress once an episode is about 80% watched
- **Rewatch aware**: shows *Rewatching* when a show is already Completed or Repeating on your list
- **Smart matching**: parses release filenames and handles `E00`-numbered releases; you can pin the right entry when detection is wrong
- **Filler warnings**: optional heads-up from [animefillerlist.com](https://www.animefillerlist.com)
- **Simulwatching**: host or join a session and mirror a friend's playback in Discord
- **Manga too**: shows what you're reading in [Suwayomi](https://github.com/Suwayomi/Suwayomi-Server)
- **Self-updating**: a one-click update check, a startup toggle and a Start Menu shortcut

## Quick start

**1.** Download `Koushin.exe` from the [latest release](https://github.com/hyuzipt/Koushin/releases/latest) and run it. There's no installer.

**2.** Turn on mpv's IPC pipe by adding this line to `%AppData%\mpv\mpv.conf`, then restart mpv:

```conf
input-ipc-server=\\.\pipe\mpv-pipe
```

**3.** Play something. Your status appears in Discord automatically.

Everything else is in the **tray menu** (right-click the icon). That's where you sign in to AniList or MyAnimeList, toggle each feature, start a Simul and check for updates.

## Simulwatching

| | |
|---|---|
| **Host** | Tray → Simulwatching → **Host a Simul…**, pick a code (3–18 characters), then send your friend the `IP:PORT` and code you're given. |
| **Join** | Tray → Simulwatching → **Join a Simul…** and paste the host's `IP:PORT` and code. Turn on **Sync my AniList** to update your own list from the host's progress. |

Koushin tries UPnP / NAT-PMP to open the port (TCP `45130` by default). If friends can't connect, forward that port on the host's router.

<details>
<summary><b>Manga (Suwayomi)</b></summary>

<br>

If a [Suwayomi](https://github.com/Suwayomi/Suwayomi-Server) server is running at `http://localhost:4567`, Koushin shows the manga and chapter you're reading, with AniList cover art. The status clears after 2 minutes without a page turn. There's nothing to set up.

</details>

<details>
<summary><b>Environment variables</b></summary>

<br>

| Variable | Default | Description |
|---|---|---|
| `MPV_PIPE` | `\\.\pipe\mpv-pipe` | mpv IPC pipe path |
| `POLL_MS` | `250` | mpv polling interval in ms (min 200) |
| `HTTP_USER_AGENT` | `koushin/…` | User agent sent with API requests |
| `SUWAYOMI_URL` | `http://localhost:4567` | Suwayomi server address |
| `SUWAYOMI_POLL_SEC` | `5` | Suwayomi polling interval (1–60) |
| `SUWAYOMI_READING_TIMEOUT_SEC` | `120` | Idle time before reading status clears (10–600) |

</details>

<details>
<summary><b>Troubleshooting</b></summary>

<br>

- **Nothing shows in Discord**: Make sure the Discord desktop app is running, *Settings → Activity Privacy → Share my activity* is on, and **Enable Discord Rich Presence** is checked in the tray.
- **mpv isn't detected**: Check the `input-ipc-server` line in `mpv.conf` and restart mpv.
- **Wrong anime is matched**: Use tray → **Select correct anime…** (there's a separate entry for MAL). Your choice is remembered for the whole series.
- **Can't connect to a Simul**: The host's port most likely isn't forwarded.

Settings, tokens, overrides and `koushin.log` are stored in `%AppData%\Koushin`.

</details>

<details>
<summary><b>Build from source</b></summary>

<br>

```bash
git clone https://github.com/hyuzipt/Koushin.git
cd Koushin
go build -trimpath -ldflags="-s -w -H=windowsgui" -o Koushin.exe
```

</details>

<br>

<div align="center"><sub>MIT License</sub></div>
