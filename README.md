# 🌙 Koushin

**Koushin** is a lightweight Windows app that connects **mpv**, **AniList**, and **Discord Rich Presence** — showing what anime you're watching in real time with rich cover art and episode progress.

<div align="center">
  <img src="https://i.imgur.com/PP2ORBq.png" width="500" alt="Koushin Demo">
</div>

---

## ✨ Features

- 🎬 Detects anime titles and episode numbers automatically from **mpv**
- 🕓 Shows episode progress and cover art directly in **Discord**
- 💫 Clickable cover image — opens the anime’s **AniList** page
- 💤 Auto-pauses Discord status when mpv is paused
- 🔗 OAuth login with AniList (per-user, saved securely)
- 🧠 Automatically updates AniList progress when ~80% of an episode is watched
- 🪶 Runs silently in the **system tray**

---

## 📥 Download

➡️ **[Latest Release](https://github.com/hyuzipt/Koushin/releases/latest)**  

Download the latest version of `Koushin.exe` and run it — no installation required.  
Make sure **Discord** and **mpv** are running in the background.

---

## 🧰 Requirements

- Windows 10/11  
- Discord (desktop app, not web)  
- mpv with JSON IPC enabled (e.g. started with `--input-ipc-server=\\.\pipe\mpv-pipe`)  
- Internet connection for AniList integration

---

## ⚙️ Build from source

```bash
git clone https://github.com/hyuzipt/Koushin.git
cd Koushin
go mod tidy
go build -ldflags="-H=windowsgui -s -w" -o Koushin.exe
