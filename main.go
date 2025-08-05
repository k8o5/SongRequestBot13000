package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
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

const audioDir = "audio_cache"
const maxSongDuration = 900 // 15 minutes in seconds

type Song struct {
	ID       string
	URL      string
	Title    string
	FilePath string
}

type Bot struct {
	config       *Config
	twitchClient *twitch.Client
	queueMutex   sync.Mutex
	songQueue    []Song
	playerCmd    *exec.Cmd
	nowPlayingID string
	ttsEnabled   bool // <-- NEU: Status für Text-to-Speech
}

type Config struct {
	BotUsername      string
	Channel          string
	OAuthToken       string
	TtsIgnoreUser    string
	OpenRouterAPIKey string
	OpenRouterModel  string
	Personality      string
}

// --- Main Application Logic ---

func main() {
	log.SetFlags(log.Ltime)

	if err := os.MkdirAll(audioDir, 0755); err != nil {
		log.Fatalf("[FATAL] Could not create audio directory: %v", err)
	}
	defer func() {
		log.Println("[CLEANUP] Deleting audio directory and all its contents...")
		os.RemoveAll(audioDir)
	}()

	cfg, err := loadConfig("config.ini")
	if err != nil {
		log.Fatalf("[FATAL] Could not load config.ini: %v", err)
	}
	bot := &Bot{
		config:     cfg,
		songQueue:  make([]Song, 0),
		ttsEnabled: true, // Standardmäßig ist TTS ausgeschaltet
	}
	go bot.downloaderLoop()
	go bot.playerLoop()
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
	log.Println("[INFO] Bot is running. Press Ctrl+C to shut down.")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("[INFO] Shutting down...")
}

// --- Twitch Command Handling ---

func (b *Bot) handleTwitchMessage(message twitch.PrivateMessage) {
	log.Printf("[CHAT] <%s> %s", message.User.DisplayName, message.Message)

	botMention := "@" + b.config.BotUsername
	// 1. Check for AI mention first
	if strings.HasPrefix(strings.ToLower(message.Message), strings.ToLower(botMention)) {
		prompt := strings.TrimSpace(strings.TrimPrefix(message.Message, botMention))
		go func() {
			response, err := b.getOpenRouterResponse(prompt)
			if err != nil {
				log.Printf("[ERROR] OpenRouter API error: %v", err)
				// b.twitchClient.Say(b.config.Channel, "Sorry, I had trouble thinking of a response.")
				return
			}
			b.twitchClient.Say(b.config.Channel, response)
		}()
		return // It's a mention, so we're done.
	}

	// 2. Check for commands
	if strings.HasPrefix(message.Message, "!") {
		parts := strings.Fields(message.Message)
		command := strings.ToLower(parts[0])
		switch command {
		case "!add":
			b.handleAdd(message)
		case "!queue":
			b.handleShowQueue()
		case "!skip":
			b.handleSkip(message)
		case "!femboy":
			b.handleFemboy(message)
		case "!help":
			b.handleHelp()
		case "!tts":
			b.handleTtsToggle(message)
		}
		return // It's a command, so we're done.
	}

	// 3. If it's not a mention or command, handle TTS
	b.queueMutex.Lock()
	isTtsOn := b.ttsEnabled
	b.queueMutex.Unlock()

	if isTtsOn &&
		strings.ToLower(message.User.Name) != strings.ToLower(b.config.TtsIgnoreUser) &&
		strings.ToLower(message.User.Name) != strings.ToLower(b.config.BotUsername) {
		go b.speakMessage(message.User.DisplayName, message.Message)
	}
}

func (b *Bot) handleTtsToggle(message twitch.PrivateMessage) {
	if !isUserMod(message) {
		b.twitchClient.Say(b.config.Channel, "You do not have permission to control TTS.")
		return
	}

	parts := strings.Fields(message.Message)
	if len(parts) < 2 {
		b.twitchClient.Say(b.config.Channel, "Usage: !tts <on|off>")
		return
	}

	b.queueMutex.Lock()
	defer b.queueMutex.Unlock()

	switch strings.ToLower(parts[1]) {
	case "on":
		b.ttsEnabled = true
		b.twitchClient.Say(b.config.Channel, "TTS is now ON.")
		log.Println("[TTS] TTS has been enabled.")
	case "off":
		b.ttsEnabled = false
		b.twitchClient.Say(b.config.Channel, "TTS is now OFF.")
		log.Println("[TTS] TTS has been disabled.")
	default:
		b.twitchClient.Say(b.config.Channel, "Usage: !tts <on|off>")
	}
}

// ... (Restliche handle-Funktionen bleiben unverändert) ...
func (b *Bot) handleAdd(message twitch.PrivateMessage) {
	parts := strings.Fields(message.Message)
	if len(parts) < 2 {
		b.twitchClient.Say(b.config.Channel, "Usage: !add <YouTube URL or search term>")
		return
	}
	query := strings.Join(parts[1:], " ")

	go func() {
		// Step 1: Find the video and validate it.
		info, err := getAndValidateVideoInfo(query)
		if err != nil {
			log.Printf("[VALIDATION] Failed for query \"%s\": %v", query, err)
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Error: %v", err))
			return
		}

		b.queueMutex.Lock()
		defer b.queueMutex.Unlock()

		// Step 2: Check for duplicates in the queue or currently playing song.
		if b.nowPlayingID == info.ID {
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf(`Song "%s" is currently playing.`, info.Title))
			return
		}
		for _, song := range b.songQueue {
			if song.ID == info.ID {
				b.twitchClient.Say(b.config.Channel, fmt.Sprintf(`Song "%s" is already in the queue.`, info.Title))
				return
			}
		}

		// Step 3: Add the validated song to the queue.
		songToAdd := Song{ID: info.ID, URL: info.URL, Title: info.Title, FilePath: ""}
		b.songQueue = append(b.songQueue, songToAdd)
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf(`Song "%s" added! Position: %d`, songToAdd.Title, len(b.songQueue)))
	}()
}

func (b *Bot) handleShowQueue() {
	b.queueMutex.Lock()
	defer b.queueMutex.Unlock()
	if len(b.songQueue) == 0 {
		b.twitchClient.Say(b.config.Channel, "The song queue is empty.")
		return
	}
	b.twitchClient.Say(b.config.Channel, "--- Current Queue ---")
	for i, song := range b.songQueue {
		if i >= 3 {
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf("...and %d more.", len(b.songQueue)-i))
			break
		}
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%d. %s", i+1, song.Title))
	}
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

func (b *Bot) handleFemboy(message twitch.PrivateMessage) {
	percentage := rand.Intn(101)
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, you are %d%% femboy!", message.User.DisplayName, percentage))
}

func (b *Bot) handleHelp() {
	b.twitchClient.Say(b.config.Channel, "Available commands: !add <url/search>, !queue, !skip (mods only), !tts <on/off> (mods only), !femboy")
}
// --- Music, Player & TTS Logic ---

// <-- NEU: Funktion zur Sprachausgabe -->
func (b *Bot) speakMessage(username, text string) {
	fullText := fmt.Sprintf("%s said %s", username, text)
	log.Printf("[TTS] Reading: %s", fullText)

	// Using espeak-ng for a fast, robotic voice.
	// -v en-us: English voice
	// -s 180: Speed in words/minute (fast)
	// -p 70: Pitch (0-99, higher is more high-pitched)
	cmd := exec.Command("espeak-ng", "-v", "en-us", "-s", "180", "-p", "70", fullText)

	// Run the command. We don't need to wait for it to finish.
	// If it fails, we just log it and move on.
	if err := cmd.Run(); err != nil {
		log.Printf("[ERROR] Failed to execute TTS command: %v", err)
	}
}

func (b *Bot) downloaderLoop() {
	for {
		var songToDownload *Song
		b.queueMutex.Lock()
		for i := range b.songQueue {
			if b.songQueue[i].FilePath == "" {
				songToDownload = &b.songQueue[i]
				break
			}
		}
		b.queueMutex.Unlock()
		if songToDownload != nil {
			log.Printf("[DOWNLOADER] Found song to download: \"%s\"", songToDownload.Title)

			filenameTemplate := audioDir + "/%(id)s.%(ext)s"

			downloadCmd := exec.Command("nice", "-n", "19", "yt-dlp",
				"-f", "bestaudio",
				"-o", filenameTemplate,
				songToDownload.URL)

			if err := downloadCmd.Run(); err != nil {
				log.Printf("[ERROR] Failed to download \"%s\": %v", songToDownload.Title, err)
			}

			getFilenameCmd := exec.Command("yt-dlp", "--get-filename", "-f", "bestaudio", "-o", filenameTemplate, songToDownload.URL)
			output, err := getFilenameCmd.Output()
			actualFilename := strings.TrimSpace(string(output))

			b.queueMutex.Lock()
			if err != nil || actualFilename == "" {
				log.Printf("[ERROR] Download failed for \"%s\". Removing from queue.", songToDownload.Title)
				b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Error: Could not download '%s'. Skipping.", songToDownload.Title))

				newQueue := make([]Song, 0, len(b.songQueue))
				for _, song := range b.songQueue {
					if song.ID != songToDownload.ID {
						newQueue = append(newQueue, song)
					}
				}
				b.songQueue = newQueue
			} else {
				log.Printf("[DOWNLOADER] Finished download for: \"%s\" -> %s", songToDownload.Title, actualFilename)
				for i := range b.songQueue {
					if b.songQueue[i].ID == songToDownload.ID {
						b.songQueue[i].FilePath = actualFilename
						break
					}
				}
			}
			b.queueMutex.Unlock()
		}
		time.Sleep(2 * time.Second)
	}
}

func (b *Bot) playerLoop() {
	for {
		var songToPlay *Song
		b.queueMutex.Lock()
		if b.playerCmd == nil && len(b.songQueue) > 0 && b.songQueue[0].FilePath != "" {
			songToPlay = &b.songQueue[0]
			b.nowPlayingID = songToPlay.ID
			b.songQueue = b.songQueue[1:]
		}
		b.queueMutex.Unlock()

		if songToPlay != nil {
			log.Printf("[PLAYER] Found ready song to play: \"%s\"", songToPlay.Title)
			b.playFile(*songToPlay)
			b.nowPlayingID = ""
		}
		time.Sleep(1 * time.Second)
	}
}

func (b *Bot) playFile(song Song) {
	defer func() {
		log.Printf("[CLEANUP] Deleting file: %s", song.FilePath)
		os.Remove(song.FilePath)
	}()

	log.Printf("▶️ Now Playing: \"%s\"", song.Title)
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Now playing: %s", song.Title))

	playerCmd := exec.Command("nice", "-n", "19", "ffplay",
		"-nodisp",
		"-autoexit",
		"-loglevel", "error",
		song.FilePath)

	playerCmd.Stdout = os.Stdout
	playerCmd.Stderr = os.Stderr

	b.queueMutex.Lock()
	b.playerCmd = playerCmd
	b.queueMutex.Unlock()

	playerCmd.Run()

	b.queueMutex.Lock()
	b.playerCmd = nil
	b.queueMutex.Unlock()

	log.Printf("⏹️ Finished Playing: \"%s\"", song.Title)
}

// --- Utility Functions ---

type YTDLPInfo struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	URL      string  `json:"webpage_url"`
	Duration float64 `json:"duration"`
	IsLive   bool    `json:"is_live"`
}

func getAndValidateVideoInfo(query string) (*YTDLPInfo, error) {
	searchQuery := query
	ytPattern := regexp.MustCompile(`(https?://)?(www\.)?(youtube|youtu|youtube-nocookie)\.(com|be)/.+`)
	if !ytPattern.MatchString(query) {
		searchQuery = "ytsearch:" + query
	}

	cmd := exec.Command("yt-dlp",
		"--quiet",
		"--dump-single-json",
		"--default-search", "ytsearch",
		searchQuery)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("could not find a video for that request")
	}

	var info YTDLPInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return nil, fmt.Errorf("failed to parse video data")
	}

	if info.IsLive {
		return nil, fmt.Errorf("cannot add live videos")
	}

	if info.Duration > maxSongDuration {
		return nil, fmt.Errorf("video is longer than 15 minutes")
	}

	if info.URL == "" && info.ID != "" {
		info.URL = fmt.Sprintf("https://www.youtube.com/watch?v=%s", info.ID)
	}

	return &info, nil
}

// --- OpenRouter API ---

type OpenRouterRequest struct {
	Model    string            `json:"model"`
	Messages []OpenRouterMessage `json:"messages"`
}

type OpenRouterMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type OpenRouterResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (b *Bot) getOpenRouterResponse(prompt string) (string, error) {
	messages := []OpenRouterMessage{
		{Role: "system", Content: b.config.Personality},
		{Role: "user", Content: prompt},
	}

	requestBody, err := json.Marshal(OpenRouterRequest{
		Model:    b.config.OpenRouterModel,
		Messages: messages,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create OpenRouter request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create http request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.config.OpenRouterAPIKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to OpenRouter: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("OpenRouter API returned non-200 status code: %d %s", resp.StatusCode, string(body))
	}

	var openRouterResponse OpenRouterResponse
	if err := json.NewDecoder(resp.Body).Decode(&openRouterResponse); err != nil {
		return "", fmt.Errorf("failed to decode OpenRouter response: %w", err)
	}

	if len(openRouterResponse.Choices) > 0 && openRouterResponse.Choices[0].Message.Content != "" {
		return openRouterResponse.Choices[0].Message.Content, nil
	}

	return "I am unable to provide a response at this time.", nil
}

func loadConfig(path string) (*Config, error) {
	file, err := ini.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := ini.Empty()
			twitchSec, _ := cfg.NewSection("Twitch")
			twitchSec.NewKey("bot_username", "your_bot_name")
			twitchSec.NewKey("channel", "your_channel_name")
			twitchSec.NewKey("oauth_token", "oauth:your_token_here")
			twitchSec.NewKey("tts_ignore_user", "some_user_to_ignore")
			orSec, _ := cfg.NewSection("OpenRouter")
			orSec.NewKey("api_key", "your_openrouter_api_key")
			orSec.NewKey("model", "your_openrouter_model")
			orSec.NewKey("personality", "You are a helpful Twitch chat bot.")
			cfg.SaveTo(path)
			return nil, fmt.Errorf("config.ini not found. A new one was created. Please fill it out.")
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	return &Config{
		BotUsername:      file.Section("Twitch").Key("bot_username").String(),
		Channel:          file.Section("Twitch").Key("channel").String(),
		OAuthToken:       file.Section("Twitch").Key("oauth_token").String(),
		TtsIgnoreUser:    file.Section("Twitch").Key("tts_ignore_user").String(),
		OpenRouterAPIKey: file.Section("OpenRouter").Key("api_key").String(),
		OpenRouterModel:  file.Section("OpenRouter").Key("model").String(),
		Personality:      file.Section("OpenRouter").Key("personality").String(),
	}, nil
}

func isUserMod(message twitch.PrivateMessage) bool {
	_, isMod := message.User.Badges["moderator"]
	_, isBroadcaster := message.User.Badges["broadcaster"]
	return isMod || isBroadcaster
}
