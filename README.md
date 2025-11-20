# 🌙 Koushin

**Koushin** is a lightweight Windows companion app that connects **mpv**, **AniList**, and **Discord Rich Presence**, showing exactly what anime you're watching — with cover art, progress, episode count, and a clean tray interface.

It runs silently in the background, updates automatically, and only requires a single `.exe`.

<div align="center">
  <img src="https://i.imgur.com/PP2ORBq.png" width="500" alt="Koushin Discord RPC Preview">
</div>

---

## ✨ Features

### 🎬 Anime Detection

* Automatically detects anime title + episode number using multiple parsers
* Works with nearly any fansub/group naming format
* Supports season matching and correct title selection

### 🔗 Discord Rich Presence

* Shows anime title, episode **(3/12 or 3/??)**
* Cover art pulled directly from AniList
* Watch progress (with automatic smoothing)
* Clickable cover → opens AniList anime page
* Optional **AniList profile badge** as the small Discord icon
* Automatically clears when no episode is playing

### 💤 Playback Awareness

* Auto-pauses Discord status when mpv is paused
* Accurate progress timer even between RPC updates
* Resets immediately when switching files or closing mpv

### 📚 AniList Integration

* Simple login using **implicit flow** — no redirect setup needed
* Token is stored locally per user
* **Auto-updates your episode progress** on AniList when ~80% is watched
* Retries safely during AniList 429 rate limits (up to 60 seconds)

### 🖥️ System Tray App

* Silent background app with a live tooltip:
  **“K-On! · Ep 3/13 · 52%”**
* Optional toggle: show/hide AniList profile icon on Discord
* Login / logout button for AniList
* **Check for updates** button
* **Auto-update** on startup if a new release is available

### 🔄 Auto-Updater

* Checks GitHub Releases automatically
* Downloads the new `.exe` safely
* Self-replaces using a temporary update script
* Cleans up update files after installing

### 🛡️ Safety / Reliability

* Prevents multiple Koushin instances from running
* Robust mpv reconnection
* Handles missing covers, API failures, timeouts, and corrupted filenames gracefully

---

## 📥 Download

👉 **[Download Latest Version](https://github.com/hyuzipt/Koushin/releases/latest)**

Just run `Koushin.exe`.
No installation, no config files needed.

---

## 🧰 Requirements

* **Windows 10/11**
* **Discord desktop app** (RPC requires the client)
* **mpv** with IPC enabled
  Example:

  ```
  mpv --input-ipc-server=\\.\pipe\mpv-pipe
  ```

---

## 📝 How to Use Koushin

Koushin works automatically — you just need to enable mpv’s IPC pipe so it can read what you’re watching.

### **1. Enable mpv IPC**

mpv must expose a JSON IPC pipe so Koushin can detect your currently playing file.

#### **Option A — Add to `mpv.conf` (Recommended)**

1. Open your mpv config folder:

   ```
   %AppData%\mpv
   ```
2. If you don’t have an `mpv.conf`, create one.
3. Add this line:

```
input-ipc-server=\\.\pipe\mpv-pipe
```

Save the file, restart mpv, and you're done.

---

#### **Option B — Launch mpv with the pipe manually**

If you prefer to drag your files into mpv directly:

```
mpv.exe --input-ipc-server=\\.\pipe\mpv-pipe
```

You can also make a shortcut:

1. Right-click `mpv.exe` → Create shortcut
2. Right-click the shortcut → Properties
3. In **Target**, append:

```
 --input-ipc-server=\\.\pipe\mpv-pipe
```

Example:

```
"E:\Apps\mpv\mpv.exe" --input-ipc-server=\\.\pipe\mpv-pipe
```

---

### **2. Run Koushin**

Just open `Koushin.exe`.
It will sit in the system tray and automatically:

* detect when mpv starts playing
* fetch the anime info
* show Discord Rich Presence

---

### **3. (Optional) Sign In to AniList**

Right-click the tray icon → **Sign in to AniList…**

This enables:

* auto-updating episode progress
* AniList profile icon in Discord (toggleable)
* better season matching and metadata

---

### **4. Done!**

From now on, whenever you watch anime in mpv, Koushin updates Discord and AniList for you — no extra steps needed.

---

## ⚙️ Build from Source

```bash
git clone https://github.com/hyuzipt/Koushin.git
cd Koushin
go mod tidy
go build -trimpath -ldflags="-s -w -H=windowsgui" -o Koushin.exe
```

---

## 🧩 How It Works

Koushin continuously polls the mpv IPC pipe, extracts metadata, resolves the anime via AniList, and pushes Discord IPC activity.

If AniList rate-limits (`429`), it shows:

`Title · Ep 3/?? · AniList rate limited, retrying in 5s`

And keeps retrying until resolved (up to ~1 minute).

---

## 📜 License

MIT License
Copyright © 2025
