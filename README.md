# 🌙 Koushin

**Koushin** is a lightweight Windows app that automatically shows what anime you're watching on **Discord** — with cover art, episode progress, and AniList integration.

Just run it in the background while you watch anime in **mpv**.

<div align="center">
  <img src="https://i.imgur.com/ksaT44h.png" width="500" alt="Koushin Discord RPC Preview">
</div>

---

## ✨ Features

- **🎬 Automatic Anime Detection** — Recognizes anime titles and episodes from any file name format
- **🔗 Discord Rich Presence** — Shows what you're watching with cover art, episode count, and progress
- **📚 AniList Sync** — Auto-updates your watch progress when you reach 80% of an episode
- **⚠️ Filler Episode Warnings** — Get notified when you're about to watch a filler episode (optional)
- **🖼️ AniList Profile Badge** — Show your AniList profile as a small icon on Discord (optional)
- **🔄 Auto-Updates** — Automatically checks for and installs new versions
- **💤 Playback Aware** — Pauses the status when mpv is paused

---

## 📥 Quick Start

### 1. Download Koushin

👉 **[Download Latest Release](https://github.com/hyuzipt/Koushin/releases/latest)**

Just download `Koushin.exe` — no installation needed.

---

### 2. Enable mpv IPC

Koushin needs mpv to expose an IPC pipe. This is a one-time setup.

**Recommended method:**

1. Press `Win + R` and type: `%AppData%\mpv`
2. Create a file named `mpv.conf` (if it doesn't exist)
3. Add this line:
```
input-ipc-server=\\.\pipe\mpv-pipe
```
4. Save and restart mpv

**Alternative (manual launch):**
```bash
mpv.exe --input-ipc-server=\\.\pipe\mpv-pipe "your-anime.mkv"
```

---

### 3. Run Koushin

Double-click `Koushin.exe`. It will appear in your system tray.

That's it! Now whenever you watch anime in mpv, Koushin will automatically update your Discord status.

---

## 🔐 AniList Login (Optional)

Right-click the Koushin tray icon → **Sign in to AniList…**

Your browser will open. Click **Approve** and you'll be automatically logged in.

**This enables:**
- ✅ Auto-updating your AniList watch progress
- ✅ Showing your AniList profile badge on Discord
- ✅ Filler episode warnings (from animefillerlist.com)

To sign out, right-click the tray icon → **Sign out of AniList**

---

## ⚙️ Settings

Right-click the Koushin tray icon to access:

| Option | Description |
|--------|-------------|
| **Show AniList profile in Discord RPC** | Displays your AniList profile as a small icon on your Discord status |
| **Warn for filler episodes** | Shows a popup when you start watching a known filler episode |
| **Check for updates…** | Manually check if a new version is available |
| **Quit** | Exit Koushin |

---

## 🛠️ Requirements

- **Windows 10/11**
- **Discord** (desktop app)
- **mpv** (media player)

---

## 🧩 How It Works

1. Koushin monitors mpv's IPC pipe for currently playing files
2. Parses the filename to extract anime title and episode number
3. Searches AniList for metadata (title, cover art, episode count)
4. Updates your Discord Rich Presence with this info
5. Auto-updates your AniList progress when you finish ~80% of an episode

---

## 🔄 Auto-Updates

Koushin automatically checks for updates on startup. If a new version is available:
- You'll see a notification
- Click **Yes** to update — Koushin will download and restart automatically

---

## ⚙️ Build from Source
```bash
git clone https://github.com/hyuzipt/Koushin.git
cd Koushin
go mod tidy
go build -trimpath -ldflags="-s -w -H=windowsgui" -o Koushin.exe
```

---

## 📜 License

MIT License © 2025

---

## ❓ Troubleshooting

**Discord status not showing?**
- Make sure Discord desktop app is running
- Check that "Display current activity as a status message" is enabled in Discord Settings → Activity Privacy

**mpv not detected?**
- Verify IPC is enabled in `mpv.conf`
- Restart mpv after adding the config line

**Anime not recognized?**
- Some file names are hard to parse — try renaming to a simpler format like `Anime Name - 01.mkv`

**Filler warnings not showing?**
- Enable "Warn for filler episodes" in the tray menu
- Not all anime have filler data available

---

<div align="center">
Made with ❤️ for the anime community
</div>
