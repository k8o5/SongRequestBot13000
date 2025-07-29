# Go Twitch CLI Song Request Bot

A simple, lightweight, 100% command-line based Twitch bot written in Go. It allows viewers to request songs from YouTube using URLs or search terms. The bot downloads the audio, plays it directly in your terminal, and pre-downloads the next song in the queue for a seamless listening experience.



## Features

- **100% CLI-Based:** No browser or web server needed. Runs entirely in your terminal.
- **Smart Song Adding:** Use `!add` with either a direct YouTube URL or a simple search term (e.g., `!add epic sax guy`).
- **Parallel Downloading:** The bot downloads the next song in the queue while the current one is playing, ensuring no delay between tracks.
- **Local Playback:** Uses `ffplay` (from FFmpeg) to play audio directly, avoiding browser complexities.
- **Automatic Cleanup:** Downloaded `.mp3` files are automatically deleted after they are played.
- **Moderation:** Basic `!skip` and `!clear` commands for the broadcaster and moderators.
- **Lightweight & Performant:** Written in Go for minimal resource usage.

## Prerequisites

You must have the following software installed and available in your system's PATH.

1.  **Go:** The programming language environment. [Installation Guide](https://go.dev/doc/install).
2.  **yt-dlp:** The command-line tool for downloading video/audio. [Installation Guide](https://github.com/yt-dlp/yt-dlp#installation).
3.  **FFmpeg:** A complete, cross-platform solution to record, convert and stream audio and video. The bot specifically uses `ffplay` for audio playback.
    -   **On Arch Linux:** `sudo pacman -S ffmpeg`
    -   **On Debian/Ubuntu:** `sudo apt install ffmpeg`
    -   **On macOS (with Homebrew):** `brew install ffmpeg`
    -   **On Windows:** [Download from the official site](https://ffmpeg.org/download.html) and add the `bin` folder to your system's PATH.

## Installation & Setup

1.  **Clone the repository:**
    ```bash
    git clone https://github.com/k8o5/SongRequestBot13000.git
    cd SongRequestBot13000
    ```

2.  **Edit the configuration file:**
    Open `config.ini` with a text editor and fill in your bot's details.
    ```bash
    nano config.ini
    ```
    You will need to provide:
    - `bot_username`: The Twitch username of your bot (e.g., `k8o5_bot`).
    - `channel`: The Twitch channel you want the bot to join (e.g., `k8o5`).
    - `oauth_token`: Your bot's OAuth token. You can get one from [twitchapps.com/tmi/](https://twitchapps.com/tmi/). **Make sure to include the `oauth:` prefix.**

3.  **Install Go dependencies:**
    The following command will read your `go.mod` file and install the necessary libraries.
    ```bash
    go mod tidy
    ```

## Running the Bot

Once the setup is complete, you can run the bot with a single command:

```bash
go run .
```
