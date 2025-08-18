package main

import (
	"bufio"
	"bytes"
	"context"
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
const maxSongDuration = 900 // 15 minutes in seconds

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
	config            *Config
	twitchClient      *twitch.Client
	queueMutex        sync.Mutex
	songQueue         []Song
	playerCmd         *exec.Cmd
	nowPlayingID      string
	downloadDir       string
	cleanupInterval   time.Duration
	chipsMutex        sync.Mutex
	userChips         map[string]int64 // NOTE: Key is now lowercase username
	freeChipsMutex    sync.Mutex
	lastFreeChips     map[string]time.Time // Key is lowercase username
	freeChipsCooldown time.Duration
	freeChipsAmount   int64
	blackjackMutex    sync.Mutex
	blackjackGames    map[string]*BlackjackGame // Key is lowercase username
	ttsEnabled        bool
	ttsMutex          sync.Mutex
	ttsPlaybackMutex  sync.Mutex
}

type Config struct {
	BotUsername  string
	Channel      string
	OAuthToken   string
	GeminiAPIKey string
	GeminiModel  string
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
		blackjackGames:    make(map[string]*BlackjackGame),
		ttsEnabled:        false,
	}

	if err := bot.loadChips(); err != nil {
		log.Printf("[WARN] Could not load user chips, starting fresh: %v", err)
	}

	go bot.downloaderLoop()
	go bot.playerLoop()
	go bot.cleanupLoop()
	go bot.periodicSaveLoop()

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

	go bot.handleTerminalInput() // Add this line

	log.Println("[INFO] Bot is running. Type commands in here or use Twitch chat. Press Ctrl+C to shut down.")
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

// --- Terminal & Twitch Command Handling ---
func (b *Bot) handleTerminalInput() {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		text, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return
			}
			log.Printf("[ERROR] Could not read from terminal: %v", err)
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}

		// Create a mock message to pass to the handler
		mockMessage := twitch.PrivateMessage{
			Channel: b.config.Channel,
			User: twitch.User{
				Name:        "TerminalUser",
				DisplayName: "TerminalUser",
			},
			Message: text,
			Tags:    make(map[string]string),
		}
		b.handleTwitchMessage(mockMessage)
	}
}

func (b *Bot) handleTwitchMessage(message twitch.PrivateMessage) {
	log.Printf("[CHAT] <%s> %s", message.User.DisplayName, message.Message)

	// TTS Handling
	b.ttsMutex.Lock()
	isTTSEnabled := b.ttsEnabled
	b.ttsMutex.Unlock()
	// Prevent bot from reading its own messages or commands if they are handled elsewhere
	if isTTSEnabled && strings.ToLower(message.User.Name) != strings.ToLower(b.config.BotUsername) {
		// We also don't want to read commands/mentions twice.
		isCommand := strings.HasPrefix(message.Message, "!")
		isMention := strings.HasPrefix(strings.ToLower(message.Message), strings.ToLower("@"+b.config.BotUsername))
		if !isCommand && !isMention {
			go b.handleTTS(message)
		}
	}

	botMention := "@" + b.config.BotUsername
	if strings.HasPrefix(strings.ToLower(message.Message), strings.ToLower(botMention)) {
		prompt := strings.TrimSpace(strings.TrimPrefix(message.Message, botMention))
		go func() {
			var response string
			var err error
			if b.config.GeminiAPIKey != "" && b.config.GeminiAPIKey != "your_gemini_api_key" {
				response, err = b.getGeminiTextResponse(prompt)
				if err != nil {
					log.Printf("[ERROR] Gemini API error: %v", err)
					return
				}
			} else {
				// No API key configured, so do nothing.
				return
			}
			b.twitchClient.Say(b.config.Channel, response)
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
	case "!tts":
		b.handleTtsToggle(message)
	}
}

func (b *Bot) handleTTS(message twitch.PrivateMessage) {
	b.ttsPlaybackMutex.Lock()
	defer b.ttsPlaybackMutex.Unlock()

	textToSpeak := fmt.Sprintf("%s says %s", message.User.DisplayName, message.Message)

	// Create a temporary file for the WAV output
	tmpfile, err := os.CreateTemp(b.downloadDir, "tts-*.wav")
	if err != nil {
		log.Printf("[ERROR] TTS: Could not create temp file: %v", err)
		return
	}
	defer os.Remove(tmpfile.Name()) // Clean up the file afterwards
	tmpfileName := tmpfile.Name()
	tmpfile.Close() // Close the file so espeak can write to it

	// Generate TTS audio using espeak
	// Using exec.Command with separate arguments prevents command injection
	espeakCmd := exec.Command("espeak", "-w", tmpfileName, textToSpeak)
	if err := espeakCmd.Run(); err != nil {
		log.Printf("[ERROR] TTS: espeak command failed: %v", err)
		// Try to inform the user in chat if espeak is likely not installed.
		if strings.Contains(err.Error(), "executable file not found") {
			log.Println("[WARN] TTS: 'espeak' command not found. Please install it to use TTS.")
			// b.twitchClient.Say(b.config.Channel, "TTS is enabled, but the 'espeak' command is not installed on the server.")
		}
		return
	}

	// Play the generated audio file with ffplay
	ffplayCmd := exec.Command("ffplay", "-nodisp", "-autoexit", "-loglevel", "error", tmpfileName)
	if err := ffplayCmd.Run(); err != nil {
		log.Printf("[ERROR] TTS: ffplay command failed: %v", err)
		return
	}
}

func (b *Bot) handleTtsToggle(message twitch.PrivateMessage) {
	if !isUserMod(message) {
		b.twitchClient.Say(b.config.Channel, "You do not have permission to toggle TTS.")
		return
	}

	b.ttsMutex.Lock()
	b.ttsEnabled = !b.ttsEnabled
	isEnabled := b.ttsEnabled
	b.ttsMutex.Unlock()

	if isEnabled {
		b.twitchClient.Say(b.config.Channel, "TTS has been enabled. Messages starting with ! will be read aloud.")
	} else {
		b.twitchClient.Say(b.config.Channel, "TTS has been disabled.")
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
	commandList := "!add, !queue, !skip, !clear, !bj, !hit, !stand, !chips, !freechips, !pay, !femboy, !tts, !help"
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

func (b *Bot) handleAdd(message twitch.PrivateMessage) {
	parts := strings.Fields(message.Message)
	if len(parts) < 2 {
		b.twitchClient.Say(b.config.Channel, "Usage: !add <YouTube URL or search term>")
		return
	}
	query := strings.Join(parts[1:], " ")

	// Check if the query is a URL or a search term.
	isSearch := false
	ytPattern := regexp.MustCompile(`(https?://)?(www\.)?(youtube|youtu|youtube-nocookie)\.(com|be)/.+`)
	if !ytPattern.MatchString(query) {
		isSearch = true
		query = "ytsearch:" + query
	}

	go func() {
		info, err := getYoutubeInfo(query)
		if err != nil {
			log.Printf("[VALIDATION] Failed for query \"%s\": %v", query, err)
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Error: %v", err))
			return
		}

		b.queueMutex.Lock()
		defer b.queueMutex.Unlock()

		// This is the logic that correctly handles search results vs. playlists.
		var songsToAdd []Song
		if isSearch {
			// If it was a search, we only want the first result, even if yt-dlp gives us a playlist.
			if len(info.Entries) > 0 {
				firstEntry := info.Entries[0]
				songsToAdd = append(songsToAdd, Song{ID: firstEntry.ID, URL: firstEntry.URL, Title: firstEntry.Title})
			} else {
				// It might be a direct video result from a search.
				songsToAdd = append(songsToAdd, Song{ID: info.ID, URL: info.URL, Title: info.Title})
			}
		} else {
			// If it was a URL, check if it's a playlist.
			if info.Type == "playlist" && len(info.Entries) > 0 {
				limit := 10
				for i, entry := range info.Entries {
					if i >= limit {
						break
					}
					songsToAdd = append(songsToAdd, Song{ID: entry.ID, URL: entry.URL, Title: entry.Title})
				}
				b.twitchClient.Say(b.config.Channel, fmt.Sprintf(`Adding %d songs from playlist "%s" (limit %d).`, len(songsToAdd), info.Title, limit))
			} else {
				// It was a single video URL.
				songsToAdd = append(songsToAdd, Song{ID: info.ID, URL: info.URL, Title: info.Title})
			}
		}

		if len(songsToAdd) == 0 {
			b.twitchClient.Say(b.config.Channel, "Could not find any songs to add from that query.")
			return
		}

		// Now, validate and add the songs.
		addedCount := 0
		for _, song := range songsToAdd {
			// Check for duplicates.
			if b.nowPlayingID == song.ID {
				continue // Skip currently playing song
			}
			isDuplicateInQueue := false
			for _, queuedSong := range b.songQueue {
				if queuedSong.ID == song.ID {
					isDuplicateInQueue = true
					break
				}
			}
			if isDuplicateInQueue {
				continue // Skip song already in queue
			}

			// Add the song.
			b.songQueue = append(b.songQueue, song)
			addedCount++
		}

		if len(songsToAdd) == 1 && addedCount == 1 {
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf(`Song "%s" added! Position: %d`, songsToAdd[0].Title, len(b.songQueue)))
		} else if addedCount > 0 {
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Added %d new songs to the queue.", addedCount))
		} else {
			b.twitchClient.Say(b.config.Channel, "All songs from that query were already in the queue or are currently playing.")
		}
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
			// Find the first song in the queue that hasn't been downloaded yet.
			if b.songQueue[i].FilePath == "" {
				songToDownload = &b.songQueue[i]
				break
			}
		}
		b.queueMutex.Unlock()

		if songToDownload != nil {
			log.Printf("[DOWNLOADER] Attempting to download: \"%s\"", songToDownload.Title)
			filenameTemplate := filepath.Join(b.downloadDir, "%(id)s.%(ext)s")

			// Create a context with a 2-minute timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			downloadCmd := exec.CommandContext(ctx, "nice", "-n", "19", "yt-dlp", "-f", "bestaudio", "-o", filenameTemplate, songToDownload.URL)

			// Run the download command.
			err := downloadCmd.Run()
			cancel() // Always call cancel to release the context resources.

			if err != nil {
				// If an error occurs, remove the song from the queue.
				log.Printf("[ERROR] Failed to download \"%s\": %v", songToDownload.Title, err)
				if ctx.Err() == context.DeadlineExceeded {
					b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Error: Download for '%s' took longer than 2 minutes and was cancelled.", songToDownload.Title))
				} else {
					b.twitchClient.Say(b.config.Channel, fmt.Sprintf("Error: Could not download '%s'.", songToDownload.Title))
				}

				// Remove the failed song from the queue.
				b.queueMutex.Lock()
				newQueue := make([]Song, 0, len(b.songQueue))
				for _, song := range b.songQueue {
					if song.ID != songToDownload.ID {
						newQueue = append(newQueue, song)
					}
				}
				b.songQueue = newQueue
				b.queueMutex.Unlock()
			} else {
				// If download is successful, get the exact filename.
				getFilenameCmd := exec.Command("yt-dlp", "--get-filename", "-f", "bestaudio", "-o", filenameTemplate, songToDownload.URL)
				output, err := getFilenameCmd.Output()
				if err != nil {
					log.Printf("[ERROR] Could not determine filename for \"%s\" after download: %v", songToDownload.Title, err)
					// Even if we can't get the filename, the file is downloaded, but we can't play it.
					// It will be cleaned up later by the cleanupLoop.
				} else {
					actualFilename := strings.TrimSpace(string(output))
					log.Printf("[DOWNLOADER] Finished download for: \"%s\" -> %s", songToDownload.Title, actualFilename)

					// Update the song in the queue with its new file path.
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
		}
		time.Sleep(2 * time.Second)
	}
}
func (b *Bot) playerLoop() {
	for {
		var songToPlay *Song
		b.queueMutex.Lock()
		// Play a song if nothing is currently playing and the first song in the queue is downloaded.
		if b.playerCmd == nil && len(b.songQueue) > 0 && b.songQueue[0].FilePath != "" {
			songToPlay = &b.songQueue[0]
			b.nowPlayingID = songToPlay.ID
			b.songQueue = b.songQueue[1:]
		}
		b.queueMutex.Unlock()

		if songToPlay != nil {
			log.Printf("[PLAYER] Found ready song to play: \"%s\"", songToPlay.Title)
			b.playFile(*songToPlay)
			// Clear the nowPlayingID after the song finishes.
			b.nowPlayingID = ""
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
	Type     string  `json:"_type"`
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	URL      string  `json:"webpage_url"`
	Duration float64 `json:"duration"`
	IsLive   bool    `json:"is_live"`
	Entries  []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"entries"`
}

func getYoutubeInfo(query string) (*YTDLPInfo, error) {
	// Note: The caller is now responsible for specifying "ytsearch:" for searches.
	cmd := exec.Command("yt-dlp", "--quiet", "--dump-single-json", query)
	output, err := cmd.Output()
	if err != nil {
		// This can happen if yt-dlp fails to find a video or another network error occurs.
		return nil, fmt.Errorf("could not find a video for that request")
	}

	var info YTDLPInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return nil, fmt.Errorf("failed to parse video data from yt-dlp: %w", err)
	}

	// If it's a playlist, yt-dlp returns a _type of "playlist".
	// We will not validate individual songs in a playlist to avoid long delays.
	if info.Type == "playlist" {
		// Populate URLs for playlist entries if they are missing
		for i := range info.Entries {
			if info.Entries[i].URL == "" {
				info.Entries[i].URL = fmt.Sprintf("https://www.youtube.com/watch?v=%s", info.Entries[i].ID)
			}
		}
		return &info, nil
	}

	// For single videos, perform the validation checks.
	if info.IsLive {
		return nil, fmt.Errorf("cannot add live videos")
	}
	if info.Duration > maxSongDuration {
		return nil, fmt.Errorf("video is longer than 15 minutes")
	}

	// Ensure the URL is populated for single videos.
	if info.URL == "" && info.ID != "" {
		info.URL = fmt.Sprintf("https://www.youtube.com/watch?v=%s", info.ID)
	}

	return &info, nil
}


// --- Gemini API ---

type GeminiRequest struct {
	Contents []GeminiContent `json:"contents"`
}

type GeminiContent struct {
	Parts []GeminiPart `json:"parts"`
}

type GeminiPart struct {
	Text       string      `json:"text,omitempty"`
	InlineData *InlineData `json:"inline_data,omitempty"`
}

type InlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

type GeminiResponse struct {
	Candidates []GeminiCandidate `json:"candidates"`
}

type GeminiCandidate struct {
	Content GeminiContent `json:"content"`
}

func (b *Bot) getGeminiTextResponse(prompt string) (string, error) {
	requestBody, err := json.Marshal(GeminiRequest{
		Contents: []GeminiContent{
			{
				Parts: []GeminiPart{
					{Text: prompt},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create Gemini request: %w", err)
	}

	url := "https://generativelanguage.googleapis.com/v1beta/models/" + b.config.GeminiModel + ":generateContent?key=" + b.config.GeminiAPIKey

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(requestBody))
	if err != nil {
		return "", fmt.Errorf("failed to create http request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to Gemini: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Gemini API returned non-200 status code: %d %s", resp.StatusCode, string(body))
	}

	var geminiResponse GeminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&geminiResponse); err != nil {
		return "", fmt.Errorf("failed to decode Gemini response: %w", err)
	}

	if len(geminiResponse.Candidates) > 0 && len(geminiResponse.Candidates[0].Content.Parts) > 0 {
		return geminiResponse.Candidates[0].Content.Parts[0].Text, nil
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
			geminiSec, _ := cfg.NewSection("Gemini")
			geminiSec.NewKey("api_key", "your_gemini_api_key")
			geminiSec.NewKey("model", "gemini-1.5-flash-latest")
			cfg.SaveTo(path)
			return nil, fmt.Errorf("config.ini not found. A new one was created. Please fill it out.")
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	return &Config{
		BotUsername: file.Section("Twitch").Key("bot_username").String(),
		Channel:     file.Section("Twitch").Key("channel").String(),
		OAuthToken:  file.Section("Twitch").Key("oauth_token").String(),
		GeminiAPIKey: file.Section("Gemini").Key("api_key").String(),
		GeminiModel: file.Section("Gemini").Key("model").String(),
	}, nil
}

func isUserMod(message twitch.PrivateMessage) bool {
	_, isMod := message.User.Badges["moderator"]
	_, isBroadcaster := message.User.Badges["broadcaster"]
	return isMod || isBroadcaster
}
