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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gempir/go-twitch-irc/v4"
	"gopkg.in/ini.v1"
)

// --- Configuration & State ---

const chipsFile = "user_chips.json"

type Song struct {
	ID       string
	URL      string
	Title    string
	FilePath string
}

type Card struct {
	Rank  string
	Suit  string
	Value int
}

type BlackjackGame struct {
	PlayerHand []Card
	DealerHand []Card
	Deck       []Card
	Bet        int64
}

type Bot struct {
	config              *Config
	twitchClient        *twitch.Client
	queueMutex          sync.Mutex
	songQueue           []Song
	playerCmd           *exec.Cmd
	downloadDir         string
	cleanupInterval     time.Duration
	chipsMutex          sync.Mutex
	userChips           map[string]int64 // NOTE: Key is now lowercase username
	freeChipsMutex      sync.Mutex
	lastFreeChips       map[string]time.Time // Key is lowercase username
	freeChipsCooldown   time.Duration
	freeChipsAmount     int64
	blackjackMutex      sync.Mutex
	blackjackGames      map[string]*BlackjackGame // Key is lowercase username
	conversationHistory []OpenRouterMessage
	freeModels          []string
	lastMessageTime     time.Time
}

type Config struct {
	BotUsername      string
	Channel          string
	OAuthToken       string
	OpenRouterAPIKey string
	OpenRouterModel  string
	Personality      string
}

// --- Main Application Logic ---

func main() {
	log.SetFlags(log.Ltime)

	downloadDir, err := os.MkdirTemp("", "musicbot-downloads")
	if err != nil {
		log.Fatalf("[FATAL] Could not create temporary download directory: %v", err)
	}
	defer os.RemoveAll(downloadDir)

	cfg, err := loadConfig("config.ini")
	if err != nil {
		log.Fatalf("[FATAL] Could not load config.ini: %v", err)
	}

	bot := &Bot{
		config:            cfg,
		songQueue:         make([]Song, 0),
		downloadDir:       downloadDir,
		cleanupInterval:   13 * time.Minute,
		userChips:         make(map[string]int64),
		lastFreeChips:     make(map[string]time.Time),
		freeChipsCooldown: 1 * time.Hour,
		freeChipsAmount:   500,
		blackjackGames:      make(map[string]*BlackjackGame),
		conversationHistory: make([]OpenRouterMessage, 0),
		freeModels:          make([]string, 0),
		lastMessageTime:     time.Now(),
	}

	if err := bot.loadChips(); err != nil {
		log.Printf("[WARN] Could not load user chips, starting fresh: %v", err)
	}

	log.Println("[INFO] Fetching free models from OpenRouter...")
	freeModels, err := getFreeModels()
	if err != nil {
		log.Printf("[WARN] Could not fetch free models from OpenRouter: %v. Idle chatter will be disabled.", err)
	} else {
		bot.freeModels = freeModels
		log.Printf("[INFO] Found %d free models.", len(bot.freeModels))
	}

	go bot.downloaderLoop()
	go bot.playerLoop()
	go bot.cleanupLoop()
	go bot.periodicSaveLoop()
	go bot.idleChatterLoop()

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
	if err := bot.saveChips(); err != nil {
		log.Printf("[ERROR] Failed to save chips on shutdown: %v", err)
	} else {
		log.Println("[INFO] User chips saved successfully.")
	}
}

// --- Twitch Command Handling ---

func (b *Bot) handleTwitchMessage(message twitch.PrivateMessage) {
	b.lastMessageTime = time.Now() // Reset idle timer on any message
	log.Printf("[CHAT] <%s> %s", message.User.DisplayName, message.Message)

	// Regex to detect the bot's name, with or without an @, as a whole word.
	mentionPattern := `(?i)\b(@)?` + regexp.QuoteMeta(b.config.BotUsername) + `\b`
	mentionRegex := regexp.MustCompile(mentionPattern)

	if mentionRegex.MatchString(message.Message) {
		// Remove the mention from the message to create the prompt.
		prompt := strings.TrimSpace(mentionRegex.ReplaceAllString(message.Message, ""))

		// BUG FIX: If the message was just the bot's name, the prompt will be empty.
		// Default to a simple greeting to ensure the bot responds.
		if prompt == "" {
			prompt = "Hello!"
		}

		go func() {
			// Add user's message to history, which will be used by getOpenRouterResponse
			b.addMessageToHistory("user", prompt)
			rawResponse, err := b.getOpenRouterResponse()
			if err != nil {
				log.Printf("[ERROR] OpenRouter API error: %v", err)
				return // Don't add a failed response to history or say anything
			}

			// Add the raw response to history so the AI has context of its tool usage
			b.addMessageToHistory("assistant", rawResponse)

			// Process the response for song adding commands and chat text
			lines := strings.Split(rawResponse, "\n")
			var chatResponseParts []string
			var songsAddedCount = 0
			for _, line := range lines {
				const addSongPrefix = "ADD_SONG:"
				if strings.HasPrefix(strings.ToUpper(line), addSongPrefix) {
					songQuery := strings.TrimSpace(line[len(addSongPrefix):])
					if songQuery != "" {
						// This is running in a goroutine, so it's safe to do the query
						_, err := b.addSongByQuery(songQuery)
						if err != nil {
							log.Printf("[AI TOOL] Error adding song '%s': %v", songQuery, err)
						} else {
							songsAddedCount++
						}
					}
				} else {
					chatResponseParts = append(chatResponseParts, line)
				}
			}

			// Send the text part of the response to chat, line by line, to handle multi-line messages.
			for _, line := range chatResponseParts {
				trimmedLine := strings.TrimSpace(line)
				if trimmedLine != "" {
					b.twitchClient.Say(b.config.Channel, trimmedLine)
				}
			}

			// Send a confirmation message if the AI added songs
			if songsAddedCount > 0 {
				plural := "song"
				if songsAddedCount > 1 {
					plural = "songs"
				}
				b.twitchClient.Say(b.config.Channel, fmt.Sprintf("k8o5bot added %d %s to the queue.", songsAddedCount, plural))
			}
		}()
		return // Don't process as a command if it's a mention
	}

	if !strings.HasPrefix(message.Message, "!") {
		return
	}

	parts := strings.Fields(message.Message)
	command := strings.ToLower(parts[0])
	switch command {
	// Music Commands
	case "!add":
		b.handleAdd(message)
	case "!queue":
		b.handleShowQueue()
	case "!skip":
		b.handleSkip(message)
	case "!clear":
		b.handleClearQueue(message)
	// Chip/Game Commands
	case "!bj", "!blackjack":
		b.handleBlackjack(message)
	case "!hit":
		b.handleHit(message)
	case "!stand":
		b.handleStand(message)
	case "!chips":
		b.handleChips(message)
	case "!freechips":
		b.handleFreeChips(message)
	case "!pay":
		b.handlePay(message)
	// Misc Commands
	case "!femboy":
		b.handleFemboy(message)
	case "!help":
		b.handleHelp(message)
	}
}

// --- Game & Chip Command Handlers ---

func (b *Bot) handlePay(message twitch.PrivateMessage) {
	parts := strings.Fields(message.Message)
	if len(parts) < 3 {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, usage: !pay <username> <amount>", message.User.DisplayName))
		return
	}

	payerUsername := strings.ToLower(message.User.Name)
	payeeUsername := strings.ToLower(strings.TrimPrefix(parts[1], "@"))

	if strings.EqualFold(payerUsername, payeeUsername) {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, you can't pay yourself!", message.User.DisplayName))
		return
	}

	amount, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || amount <= 0 {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, please enter a valid positive number to pay.", message.User.DisplayName))
		return
	}

	b.chipsMutex.Lock()
	defer b.chipsMutex.Unlock()

	payerChips := b.userChips[payerUsername]
	if payerChips < amount {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, you don't have enough chips to pay that amount. You have %d.", message.User.DisplayName, payerChips))
		return
	}

	b.userChips[payerUsername] -= amount
	b.userChips[payeeUsername] += amount

	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s paid %s %d chips!", message.User.DisplayName, payeeUsername, amount))
}

func (b *Bot) handleBlackjack(message twitch.PrivateMessage) {
	parts := strings.Fields(message.Message)
	if len(parts) < 2 {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, usage: !bj <bet_amount>", message.User.DisplayName))
		return
	}
	betAmount, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || betAmount <= 0 {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, please enter a valid positive number to bet.", message.User.DisplayName))
		return
	}
	if betAmount > 10000 {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, the maximum bet is 10,000 chips.", message.User.DisplayName))
		return
	}

	username := strings.ToLower(message.User.Name)
	b.blackjackMutex.Lock()
	if _, ok := b.blackjackGames[username]; ok {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, you already have a game in progress. Use !hit or !stand.", message.User.DisplayName))
		b.blackjackMutex.Unlock()
		return
	}
	b.blackjackMutex.Unlock()

	b.chipsMutex.Lock()
	userChips := b.userChips[username]
	if userChips < betAmount {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, you don't have enough chips! You have %d.", message.User.DisplayName, userChips))
		b.chipsMutex.Unlock()
		return
	}
	b.userChips[username] -= betAmount
	b.chipsMutex.Unlock()

	deck := newDeck()
	shuffleDeck(deck)
	game := &BlackjackGame{PlayerHand: make([]Card, 0), DealerHand: make([]Card, 0), Deck: deck, Bet: betAmount}
	game.PlayerHand = append(game.PlayerHand, dealCard(&game.Deck))
	game.DealerHand = append(game.DealerHand, dealCard(&game.Deck))
	game.PlayerHand = append(game.PlayerHand, dealCard(&game.Deck))
	game.DealerHand = append(game.DealerHand, dealCard(&game.Deck))

	b.blackjackMutex.Lock()
	b.blackjackGames[username] = game
	b.blackjackMutex.Unlock()

	playerValue, _ := calculateHandValue(game.PlayerHand)
	dealerValue, _ := calculateHandValue(game.DealerHand)
	playerHandStr := handToString(game.PlayerHand, playerValue)
	dealerHandStr := fmt.Sprintf("[%s, ?]", game.DealerHand[0].Rank+game.DealerHand[0].Suit)

	if playerValue == 21 {
		if dealerValue == 21 { // Push
			b.endBlackjackGame(message, "Both you and the dealer have Blackjack! It's a push.", game.Bet)
		} else { // Player Blackjack
			payout := game.Bet + (game.Bet * 3 / 2)
			b.endBlackjackGame(message, fmt.Sprintf("Blackjack! You win %d chips!", payout), payout)
		}
		return
	}
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s started a game! Your hand: %s. Dealer shows: %s. Use !hit or !stand.", message.User.DisplayName, playerHandStr, dealerHandStr))
}

func (b *Bot) handleHit(message twitch.PrivateMessage) {
	username := strings.ToLower(message.User.Name)

	b.blackjackMutex.Lock()
	game, ok := b.blackjackGames[username]
	if !ok {
		b.blackjackMutex.Unlock()
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, you don't have a game in progress. Use !bj <amount> to start one.", message.User.DisplayName))
		return
	}
	b.blackjackMutex.Unlock()

	game.PlayerHand = append(game.PlayerHand, dealCard(&game.Deck))
	playerValue, _ := calculateHandValue(game.PlayerHand)
	playerHandStr := handToString(game.PlayerHand, playerValue)

	if playerValue > 21 {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s busts with %s! You lose %d chips.", message.User.DisplayName, playerHandStr, game.Bet))
		b.endBlackjackGame(message, "", 0) // End game with 0 payout
		return
	}

	if playerValue == 21 {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, your new hand: %s. Automatically standing.", message.User.DisplayName, playerHandStr))
		b.handleStand(message) // Automatically stand for the player
		return
	}

	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, your new hand: %s. !hit or !stand?", message.User.DisplayName, playerHandStr))
}

func (b *Bot) handleStand(message twitch.PrivateMessage) {
	username := strings.ToLower(message.User.Name)

	b.blackjackMutex.Lock()
	game, ok := b.blackjackGames[username]
	if !ok {
		b.blackjackMutex.Unlock()
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, you don't have a game in progress. Use !bj <amount> to start one.", message.User.DisplayName))
		return
	}
	b.blackjackMutex.Unlock()

	playerValue, _ := calculateHandValue(game.PlayerHand)
	playerHandStr := handToString(game.PlayerHand, playerValue)

	for {
		dealerValue, _ := calculateHandValue(game.DealerHand)
		if dealerValue >= 17 {
			break
		}
		game.DealerHand = append(game.DealerHand, dealCard(&game.Deck))
	}

	dealerValue, _ := calculateHandValue(game.DealerHand)
	dealerHandStr := handToString(game.DealerHand, dealerValue)

	resultMsg := fmt.Sprintf("Your hand: %s. Dealer's hand: %s. ", playerHandStr, dealerHandStr)
	var payout int64
	var outcome string

	if dealerValue > 21 {
		payout = game.Bet * 2
		outcome = fmt.Sprintf("Dealer busts! You win %d chips!", payout)
	} else if playerValue > dealerValue {
		payout = game.Bet * 2
		outcome = fmt.Sprintf("You win %d chips!", payout)
	} else if playerValue < dealerValue {
		payout = 0
		outcome = fmt.Sprintf("You lose %d chips.", game.Bet)
	} else {
		payout = game.Bet
		outcome = "It's a push! Your bet is returned."
	}
	b.endBlackjackGame(message, resultMsg+outcome, payout)
}

// --- Other Command Handlers ---

func (b *Bot) handleHelp(message twitch.PrivateMessage) {
	commandList := "!add, !queue, !skip, !clear, !bj, !hit, !stand, !chips, !freechips, !pay, !femboy, !help"
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Available commands: %s", commandList))
}

func (b *Bot) handleChips(message twitch.PrivateMessage) {
	b.chipsMutex.Lock()
	defer b.chipsMutex.Unlock()
	username := strings.ToLower(message.User.Name)
	balance := b.userChips[username]
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, you have %d chips.", message.User.DisplayName, balance))
}

func (b *Bot) handleFreeChips(message twitch.PrivateMessage) {
	username := strings.ToLower(message.User.Name)
	b.freeChipsMutex.Lock()
	lastClaim, hasClaimed := b.lastFreeChips[username]
	if hasClaimed && time.Since(lastClaim) < b.freeChipsCooldown {
		remaining := b.freeChipsCooldown - time.Since(lastClaim)
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, you can claim free chips again in %v.", message.User.DisplayName, remaining.Round(time.Second)))
		b.freeChipsMutex.Unlock()
		return
	}
	b.lastFreeChips[username] = time.Now()
	b.freeChipsMutex.Unlock()

	b.chipsMutex.Lock()
	b.userChips[username] += b.freeChipsAmount
	newBalance := b.userChips[username]
	b.chipsMutex.Unlock()

	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, here are %d free chips! Your new balance is %d.", message.User.DisplayName, b.freeChipsAmount, newBalance))
}

// addSongByQuery is a helper that fetches song info and adds it to the queue.
// It returns the display title of what was added or an error.
func (b *Bot) addSongByQuery(query string) (string, error) {
	ytPattern := regexp.MustCompile(`(https?://)?(www\.)?(youtube|youtu|youtube-nocookie)\.(com|be)/.+`)
	if !ytPattern.MatchString(query) {
		query = "ytsearch:" + query
	}

	info, err := getYoutubeInfo(query)
	if err != nil {
		log.Printf("[ERROR] Could not get video info for query '%s': %v", query, err)
		return "", fmt.Errorf("could not find a video for that request")
	}

	b.queueMutex.Lock()
	defer b.queueMutex.Unlock()

	var addedTitle string
	if len(info.Entries) > 0 {
		if strings.HasPrefix(query, "ytsearch:") {
			entry := info.Entries[0]
			songToAdd := Song{ID: entry.ID, URL: entry.URL, Title: entry.Title, FilePath: ""}
			b.songQueue = append(b.songQueue, songToAdd)
			addedTitle = songToAdd.Title
		} else { // Playlist
			count := 0
			for i, entry := range info.Entries {
				if i >= 10 { // Limit playlist adds to 10
					break
				}
				b.songQueue = append(b.songQueue, Song{ID: entry.ID, URL: entry.URL, Title: entry.Title, FilePath: ""})
				count++
			}
			addedTitle = fmt.Sprintf(`playlist "%s" (%d songs)`, info.Title, count)
		}
	} else { // Single video
		songToAdd := Song{ID: info.ID, URL: info.URL, Title: info.Title, FilePath: ""}
		b.songQueue = append(b.songQueue, songToAdd)
		addedTitle = songToAdd.Title
	}
	return addedTitle, nil
}

func (b *Bot) handleAdd(message twitch.PrivateMessage) {
	parts := strings.Fields(message.Message)
	if len(parts) < 2 {
		b.twitchClient.Say(b.config.Channel, "Usage: !add <YouTube URL or search term>")
		return
	}
	query := strings.Join(parts[1:], " ")

	go func() {
		addedTitle, err := b.addSongByQuery(query)
		if err != nil {
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Error: %v", err))
			return
		}

		b.queueMutex.Lock()
		position := len(b.songQueue)
		b.queueMutex.Unlock()
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf(`Added: %s (Position: %d)`, addedTitle, position))
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
// ... (This section is unchanged, so I'll omit it for brevity) ...
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
			filenameTemplate := filepath.Join(b.downloadDir, "%(id)s.%(ext)s")
			downloadCmd := exec.Command("nice", "-n", "19", "yt-dlp", "-f", "bestaudio", "-o", filenameTemplate, songToDownload.URL)
			if err := downloadCmd.Run(); err != nil {
				log.Printf("[ERROR] Failed to download \"%s\": %v", songToDownload.Title, err)
			} else {
				getFilenameCmd := exec.Command("yt-dlp", "--get-filename", "-f", "bestaudio", "-o", filenameTemplate, songToDownload.URL)
				output, err := getFilenameCmd.Output()
				if err != nil {
					log.Printf("[ERROR] Could not determine filename for \"%s\": %v", songToDownload.Title, err)
					continue
				}
				actualFilename := strings.TrimSpace(string(output))
				log.Printf("[DOWNLOADER] Finished download for: \"%s\" -> %s", songToDownload.Title, actualFilename)
				b.queueMutex.Lock()
				for i := range b.songQueue {
					if b.songQueue[i].ID == songToDownload.ID {
						b.songQueue[i].FilePath = actualFilename
						break
					}
				}
				b.queueMutex.Unlock()
			}
		}
		time.Sleep(2 * time.Second)
	}
}
func (b *Bot) playerLoop() {
	for {
		var songToPlay *Song
		b.queueMutex.Lock()
		if len(b.songQueue) > 0 && b.songQueue[0].FilePath != "" {
			songToPlay = &b.songQueue[0]
			b.songQueue = b.songQueue[1:]
		}
		b.queueMutex.Unlock()
		if songToPlay != nil {
			log.Printf("[PLAYER] Found ready song to play: \"%s\"", songToPlay.Title)
			b.playFile(*songToPlay)
		}
		time.Sleep(1 * time.Second)
	}
}
func (b *Bot) playFile(song Song) {
	log.Printf("▶️ Now Playing: \"%s\"", song.Title)
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Now playing: %s", song.Title))
	playerCmd := exec.Command("nice", "-n", "19", "ffplay", "-nodisp", "-autoexit", "-loglevel", "error", song.FilePath)
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
func (b *Bot) cleanupLoop() {
	ticker := time.NewTicker(b.cleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		log.Printf("[CLEANUP] Scanning for files older than %v in %s", b.cleanupInterval, b.downloadDir)
		files, err := os.ReadDir(b.downloadDir)
		if err != nil {
			log.Printf("[ERROR] Cleanup failed: Could not read download directory: %v", err)
			continue
		}
		for _, file := range files {
			filePath := filepath.Join(b.downloadDir, file.Name())
			info, err := file.Info()
			if err != nil {
				log.Printf("[ERROR] Cleanup failed: Could not get file info for %s: %v", filePath, err)
				continue
			}
			if time.Since(info.ModTime()) > b.cleanupInterval {
				log.Printf("[CLEANUP] Deleting old file: %s", filePath)
				err := os.Remove(filePath)
				if err != nil {
					log.Printf("[ERROR] Cleanup failed: Could not delete file %s: %v", filePath, err)
				}
			}
		}
	}
}

// --- Utility & Persistence Functions ---

const maxHistorySize = 10 // Keep the last 10 messages (5 pairs of user/bot)

func (b *Bot) addMessageToHistory(role, content string) {
	b.conversationHistory = append(b.conversationHistory, OpenRouterMessage{Role: role, Content: content})
	if len(b.conversationHistory) > maxHistorySize {
		// Keep the history size bounded by removing the oldest message.
		b.conversationHistory = b.conversationHistory[len(b.conversationHistory)-maxHistorySize:]
	}
}

func (b *Bot) endBlackjackGame(message twitch.PrivateMessage, resultMessage string, payout int64) {
	username := strings.ToLower(message.User.Name)
	b.blackjackMutex.Lock()
	defer b.blackjackMutex.Unlock()
	b.chipsMutex.Lock()
	defer b.chipsMutex.Unlock()

	// Only modify chips and send a message if there's a result to report.
	// This prevents duplicate messages when called from handleHit for a bust.
	if resultMessage != "" {
		b.userChips[username] += payout
		finalMsg := fmt.Sprintf("%s, %s Your new balance is %d.", message.User.DisplayName, resultMessage, b.userChips[username])
		b.twitchClient.Say(b.config.Channel, finalMsg)
	} else if payout == 0 { // This is a bust scenario
		b.userChips[username] += 0 // Payout is 0 on a bust, chips are already deducted.
		finalMsg := fmt.Sprintf("Your new balance is %d.", b.userChips[username])
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, %s", message.User.DisplayName, finalMsg))
	}

	delete(b.blackjackGames, username)
}

func newDeck() []Card {
	suits := []string{"♥", "♦", "♣", "♠"}
	ranks := []string{"2", "3", "4", "5", "6", "7", "8", "9", "10", "J", "Q", "K", "A"}
	var deck []Card
	for _, suit := range suits {
		for _, rank := range ranks {
			value := 0
			switch rank {
			case "J", "Q", "K":
				value = 10
			case "A":
				value = 11
			default:
				value, _ = strconv.Atoi(rank)
			}
			deck = append(deck, Card{Rank: rank, Suit: suit, Value: value})
		}
	}
	return deck
}

func shuffleDeck(deck []Card) {
	rand.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
}

func dealCard(deck *[]Card) Card {
	card := (*deck)[0]
	*deck = (*deck)[1:]
	return card
}

func calculateHandValue(hand []Card) (int, int) {
	value, aces := 0, 0
	for _, card := range hand {
		value += card.Value
		if card.Rank == "A" {
			aces++
		}
	}
	for value > 21 && aces > 0 {
		value -= 10
		aces--
	}
	return value, len(hand)
}

func handToString(hand []Card, value int) string {
	var parts []string
	for _, card := range hand {
		parts = append(parts, card.Rank+card.Suit)
	}
	return fmt.Sprintf("[%s] (%d)", strings.Join(parts, ", "), value)
}

func (b *Bot) periodicSaveLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if err := b.saveChips(); err != nil {
			log.Printf("[ERROR] Failed to periodically save chips: %v", err)
		} else {
			log.Println("[INFO] User chips saved periodically.")
		}
	}
}

func (b *Bot) saveChips() error {
	b.chipsMutex.Lock()
	defer b.chipsMutex.Unlock()
	data, err := json.MarshalIndent(b.userChips, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal chips to JSON: %w", err)
	}
	return os.WriteFile(chipsFile, data, 0644)
}

func (b *Bot) loadChips() error {
	b.chipsMutex.Lock()
	defer b.chipsMutex.Unlock()
	file, err := os.Open(chipsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to open chips file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("failed to read chips file: %w", err)
	}
	return json.Unmarshal(data, &b.userChips)
}

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

// --- OpenRouter API ---

type OpenRouterModel struct {
	ID      string `json:"id"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
}

type OpenRouterModelsResponse struct {
	Data []OpenRouterModel `json:"data"`
}

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

func getFreeModels() ([]string, error) {
	req, err := http.NewRequest("GET", "https://openrouter.ai/api/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create models request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to OpenRouter for models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("OpenRouter models API returned non-200 status: %d %s", resp.StatusCode, string(body))
	}

	var modelsResponse OpenRouterModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResponse); err != nil {
		return nil, fmt.Errorf("failed to decode OpenRouter models response: %w", err)
	}

	var freeModels []string
	for _, model := range modelsResponse.Data {
		// As per the user's script, we check for "0" strings.
		if model.Pricing.Prompt == "0" && model.Pricing.Completion == "0" {
			freeModels = append(freeModels, model.ID)
		}
	}

	return freeModels, nil
}

func (b *Bot) getOpenRouterResponse() (string, error) {
	// Prepare the messages for the API call, including history and system prompt.
	messages := []OpenRouterMessage{}

	// Add the system prompt first.
	systemPrompt := b.config.Personality
	if strings.ToLower(b.config.BotUsername) == "k8o5bot" {
		systemPrompt = "You are k8o5bot, a witty and helpful AI assistant in a Twitch chat. You are knowledgeable about gaming, programming, and internet culture. You are multilingual and should always respond in the same language as the user's prompt. Keep your responses concise and engaging. The channel broadcaster is k8o5. You have a tool to add songs to the queue. If a user asks you to create a playlist or add a song, respond with `ADD_SONG: <song name or youtube url>` on a new line for each song. Example: 'Sure, here's a chill playlist for you:\\nADD_SONG: lofi hip hop radio\\nADD_SONG: ChilledCow' - The user will not see the `ADD_SONG:` lines, just your text."
	}
	if systemPrompt != "" {
		messages = append(messages, OpenRouterMessage{Role: "system", Content: systemPrompt})
	}

	// Add the conversation history.
	messages = append(messages, b.conversationHistory...)

	// Create the request body.
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

	if len(openRouterResponse.Choices) > 0 {
		return openRouterResponse.Choices[0].Message.Content, nil
	}

	return "Sorry, I couldn't come up with a response.", nil
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

// --- Idle Chatter ---

// getOpenRouterResponseForModel is a simplified version of getOpenRouterResponse that doesn't use
// conversation history and allows specifying a model. Used by the idle chatter.
func (b *Bot) getOpenRouterResponseForModel(prompt, modelName string) (string, error) {
	requestBody, err := json.Marshal(OpenRouterRequest{
		Model: modelName,
		Messages: []OpenRouterMessage{
			{Role: "user", Content: prompt},
		},
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

	return "", fmt.Errorf("received an empty response from the model")
}

func (b *Bot) idleChatterLoop() {
	// A ticker that checks every 30 seconds.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	idleThreshold := 5 * time.Minute
	nextPrompt := "Tell me a surprising fact about the ocean."
	modelIndex := 0

	for range ticker.C {
		if len(b.freeModels) == 0 {
			// No free models loaded, so we can't do anything.
			// Sleep for the idle threshold to avoid checking every 30s.
			time.Sleep(idleThreshold)
			continue
		}

		if time.Since(b.lastMessageTime) > idleThreshold {
			// Pick the next model in the list, looping around.
			modelIndex = (modelIndex + 1) % len(b.freeModels)
			selectedModel := b.freeModels[modelIndex]

			log.Printf("[IDLE CHATTER] Chat idle for over %v. Sending prompt to model '%s'.", idleThreshold, selectedModel)

			response, err := b.getOpenRouterResponseForModel(nextPrompt, selectedModel)
			if err != nil {
				log.Printf("[IDLE CHATTER] Error from model %s: %v. Skipping.", selectedModel, err)
				// Try the next model on the next tick with the same prompt.
				continue
			}

			// The successful response becomes the prompt for the next model.
			nextPrompt = response

			b.twitchClient.Say(b.config.Channel, response)

			// Update the last message time to now. This creates a pause equal to the
			// idleThreshold before the *next* idle message is sent, preventing spam.
			b.lastMessageTime = time.Now()
		}
	}
}
