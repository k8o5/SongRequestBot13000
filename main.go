package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gempir/go-twitch-irc/v4"
	"gopkg.in/ini.v1"
)

// --- Configuration & State ---

// Song struct now includes FilePath to track download status.
// If FilePath is empty, it needs to be downloaded.
// If FilePath is not empty, it's ready to be played.
type Song struct {
	ID       string
	URL      string
	Title    string
	FilePath string
}

// Bot struct now manages the two new loops.
type Bot struct {
	config       *Config
	twitchClient *twitch.Client

	queueMutex sync.Mutex
	songQueue  []Song

	// playerCmd holds the currently running ffplay process so we can kill it to skip
	playerCmd *exec.Cmd
}

type Config struct {
	BotUsername string
	Channel     string
	OAuthToken  string
}

// --- Main Application Logic ---

func main() {
	log.SetFlags(log.Ltime)

	cfg, err := loadConfig("config.ini")
	if err != nil {
		log.Fatalf("[FATAL] Could not load config.ini: %v", err)
	}

	bot := &Bot{
		config:    cfg,
		songQueue: make([]Song, 0),
	}

	// Start the two main loops in the background. They will run concurrently.
	go bot.downloaderLoop()
	go bot.playerLoop()

	// Configure and start the Twitch client
	bot.twitchClient = twitch.NewClient(cfg.BotUsername, cfg.OAuthToken)
	bot.twitchClient.OnPrivateMessage(bot.handleTwitchMessage)
	bot.twitchClient.OnConnect(func() {
		log.Println("✅ ✅ ✅ SUCCESS: Connected to Twitch chat!")
		bot.twitchClient.Join(cfg.Channel)
		log.Printf("✅ ✅ ✅ SUCCESS: Joined channel #%s. Bot is fully operational.", cfg.Channel)
	})

	go func() {
		log.Println("[INFO] Attempting to connect to Twitch...")
		err := bot.twitchClient.Connect()
		if err != nil {
			log.Printf("❌ ❌ ❌ FAILED: Twitch connection error: %v", err)
		}
	}()

	// Block until Ctrl+C is pressed
	log.Println("[INFO] Bot is running. Press Ctrl+C to shut down.")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("[INFO] Shutting down...")
}

// --- Twitch Command Handling ---

func (b *Bot) handleTwitchMessage(message twitch.PrivateMessage) {
	log.Printf("[CHAT] <%s> %s", message.User.DisplayName, message.Message)
	if !strings.HasPrefix(message.Message, "!") {
		return
	}
	parts := strings.Fields(message.Message)
	command := strings.ToLower(parts[0])

	switch command {
	case "!add":
		b.handleAdd(message)
	case "!queue":
		b.handleShowQueue()
	case "!skip":
		b.handleSkip(message)
	case "!clear":
		b.handleClearQueue(message)
	case "!femboy":
		b.handleFemboy(message)
	}
}

func (b *Bot) handleAdd(message twitch.PrivateMessage) {
	parts := strings.Fields(message.Message)
	if len(parts) < 2 {
		b.twitchClient.Say(b.config.Channel, "Usage: !add <YouTube URL or search term>")
		return
	}
	query := strings.Join(parts[1:], " ")

	ytPattern := regexp.MustCompile(`(https?://)?(www\.)?(youtube|youtu|youtube-nocookie)\.(com|be)/.+`)
	if !ytPattern.MatchString(query) {
		query = "ytsearch:" + query
	}

	go func() {
		info, err := getYoutubeInfo(query)
		if err != nil {
			log.Printf("[ERROR] Could not get video info: %v", err)
			b.twitchClient.Say(b.config.Channel, "Error: Could not find a video for that request.")
			return
		}

		b.queueMutex.Lock()
		defer b.queueMutex.Unlock()

		if len(info.Entries) > 0 { // Playlist or search result
			if strings.HasPrefix(query, "ytsearch:") {
				entry := info.Entries[0]
				// Add the song with an EMPTY FilePath. The downloader will pick it up.
				songToAdd := Song{ID: entry.ID, URL: entry.URL, Title: entry.Title, FilePath: ""}
				b.songQueue = append(b.songQueue, songToAdd)
				b.twitchClient.Say(b.config.Channel, fmt.Sprintf(`Song "%s" added! Position: %d`, songToAdd.Title, len(b.songQueue)))
			} else {
				count := 0
				for i, entry := range info.Entries {
					if i >= 10 {
						break
					}
					b.songQueue = append(b.songQueue, Song{ID: entry.ID, URL: entry.URL, Title: entry.Title, FilePath: ""})
					count++
				}
				b.twitchClient.Say(b.config.Channel, fmt.Sprintf(`Playlist "%s" added with %d songs!`, info.Title, count))
			}
		} else { // Single video URL
			songToAdd := Song{ID: info.ID, URL: info.URL, Title: info.Title, FilePath: ""}
			b.songQueue = append(b.songQueue, songToAdd)
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf(`Song "%s" added! Position: %d`, songToAdd.Title, len(b.songQueue)))
		}
	}()
}

func (b *Bot) handleShowQueue() {
	b.queueMutex.Lock()
	defer b.queueMutex.Unlock()
	if len(b.songQueue) == 0 {
		log.Println("[QUEUE] The queue is empty.")
		b.twitchClient.Say(b.config.Channel, "The song queue is empty.")
		return
	}
	log.Println("--- Current Queue ---")
	for i, song := range b.songQueue {
		status := "Queued"
		if song.FilePath != "" {
			status = "Ready"
		}
		log.Printf("%d. %s [%s]", i+1, song.Title, status)
	}
	log.Println("---------------------")
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("There are %d songs in the queue. The full list is in my terminal.", len(b.songQueue)))
}

func (b *Bot) handleSkip(message twitch.PrivateMessage) {
	if !isUserMod(message) {
		b.twitchClient.Say(b.config.Channel, "You do not have permission to skip songs.")
		return
	}
	b.queueMutex.Lock()
	defer b.queueMutex.Unlock()
	if b.playerCmd != nil && b.playerCmd.Process != nil {
		log.Println("[PLAYER] Skip requested. Terminating current song...")
		b.playerCmd.Process.Kill()
	} else {
		log.Println("[PLAYER] Skip requested, but nothing is playing.")
	}
}

func (b *Bot) handleClearQueue(message twitch.PrivateMessage) {
	if !isUserMod(message) {
		b.twitchClient.Say(b.config.Channel, "You do not have permission to clear the queue.")
		return
	}
	b.queueMutex.Lock()
	b.songQueue = make([]Song, 0)
	b.queueMutex.Unlock()
	b.twitchClient.Say(b.config.Channel, "The song queue has been cleared.")
	log.Println("[QUEUE] Queue cleared by moderator.")
}

func (b *Bot) handleFemboy(message twitch.PrivateMessage) {
	percentage := rand.Intn(101)
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, you are %d%% femboy!", message.User.DisplayName, percentage))
}

// --- Music & Player Logic ---

// downloaderLoop continuously checks the queue for songs that need downloading.
func (b *Bot) downloaderLoop() {
	for {
		var songToDownload *Song

		// --- Find a song to download ---
		b.queueMutex.Lock()
		// We look for the first song in the queue that has not yet been downloaded.
		for i := range b.songQueue {
			if b.songQueue[i].FilePath == "" {
				songToDownload = &b.songQueue[i]
				break
			}
		}
		b.queueMutex.Unlock()
		// ---------------------------------

		if songToDownload != nil {
			log.Printf("[DOWNLOADER] Found song to download: \"%s\"", songToDownload.Title)
			filename := fmt.Sprintf("%s.mp3", songToDownload.ID)

			// Download the song. This is a blocking operation.
			downloadCmd := exec.Command("yt-dlp", "-x", "--audio-format", "mp3", "-o", filename, songToDownload.URL)
			if err := downloadCmd.Run(); err != nil {
				log.Printf("[ERROR] Failed to download \"%s\": %v", songToDownload.Title, err)
				// We could implement logic here to remove the failed song from the queue.
			} else {
				log.Printf("[DOWNLOADER] Finished download for: \"%s\"", songToDownload.Title)
				// --- Mark the song as ready ---
				b.queueMutex.Lock()
				// Find the song again in case the queue has changed and update its FilePath.
				for i := range b.songQueue {
					if b.songQueue[i].ID == songToDownload.ID {
						b.songQueue[i].FilePath = filename
						break
					}
				}
				b.queueMutex.Unlock()
				// ------------------------------
			}
		}
		// Wait a moment before checking the queue again to avoid busy-looping.
		time.Sleep(2 * time.Second)
	}
}

// playerLoop continuously checks if it can play a downloaded song.
func (b *Bot) playerLoop() {
	for {
		// --- Find a song to play ---
		var songToPlay *Song
		b.queueMutex.Lock()
		// We only play if nothing else is playing AND the queue is not empty
		// AND the first song in the queue is ready (has a FilePath).
		if b.playerCmd == nil && len(b.songQueue) > 0 && b.songQueue[0].FilePath != "" {
			// Take the song from the queue
			songToPlay = &b.songQueue[0]
			b.songQueue = b.songQueue[1:]
		}
		b.queueMutex.Unlock()
		// ---------------------------

		if songToPlay != nil {
			log.Printf("[PLAYER] Found ready song to play: \"%s\"", songToPlay.Title)
			b.playFile(*songToPlay)
		}

		// Wait a moment before checking again.
		time.Sleep(1 * time.Second)
	}
}

// playFile plays a single, already-downloaded audio file.
func (b *Bot) playFile(song Song) {
	// Defer the cleanup. This will run after playback finishes or is skipped.
	defer func() {
		log.Printf("[CLEANUP] Deleting file: %s", song.FilePath)
		os.Remove(song.FilePath)
	}()

	log.Printf("▶️ Now Playing: \"%s\"", song.Title)
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Now playing: %s", song.Title))

	playerCmd := exec.Command("ffplay", "-nodisp", "-autoexit", song.FilePath)

	// Store the command so !skip can kill it.
	b.queueMutex.Lock()
	b.playerCmd = playerCmd
	b.queueMutex.Unlock()

	// This blocks until the song is over or the process is killed.
	playerCmd.Run()

	// Clear the command after it's done so the playerLoop knows it can play the next song.
	b.queueMutex.Lock()
	b.playerCmd = nil
	b.queueMutex.Unlock()

	log.Printf("⏹️ Finished Playing: \"%s\"", song.Title)
}

// --- Utility Functions ---

type YTDLPInfo struct {
	Type    string `json:"_type"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	URL     string `json:"webpage_url"`
	Entries []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"entries"`
}

func getYoutubeInfo(query string) (*YTDLPInfo, error) {
	cmd := exec.Command("yt-dlp", "--quiet", "--dump-single-json", "--flat-playlist", query)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp execution failed: %w", err)
	}
	var info YTDLPInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return nil, fmt.Errorf("failed to parse yt-dlp JSON: %w", err)
	}
	if info.Type == "playlist" && len(info.Entries) > 0 {
		for i := range info.Entries {
			if info.Entries[i].URL == "" {
				info.Entries[i].URL = fmt.Sprintf("https://www.youtube.com/watch?v=%s", info.Entries[i].ID)
			}
		}
	} else if info.URL == "" && info.ID != "" {
		info.URL = fmt.Sprintf("https://www.youtube.com/watch?v=%s", info.ID)
	}
	return &info, nil
}

func loadConfig(path string) (*Config, error) {
	file, err := ini.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := ini.Empty()
			sec, _ := cfg.NewSection("Twitch")
			sec.NewKey("bot_username", "your_bot_name")
			sec.NewKey("channel", "your_channel_name")
			sec.NewKey("oauth_token", "oauth:your_token_here")
			cfg.SaveTo(path)
			return nil, fmt.Errorf("config.ini not found. A new one was created. Please fill it out.")
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	return &Config{
		BotUsername: file.Section("Twitch").Key("bot_username").String(),
		Channel:     file.Section("Twitch").Key("channel").String(),
		OAuthToken:  file.Section("Twitch").Key("oauth_token").String(),
	}, nil
}

func isUserMod(message twitch.PrivateMessage) bool {
	_, isMod := message.User.Badges["moderator"]
	_, isBroadcaster := message.User.Badges["broadcaster"]
	return isMod || isBroadcaster
}
