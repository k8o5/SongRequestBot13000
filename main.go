package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
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
	config            *Config
	twitchClient      *twitch.Client
	queueMutex        sync.Mutex
	songQueue         []Song
	playerCmd         *exec.Cmd
	downloadDir       string
	cleanupInterval   time.Duration
	chipsMutex        sync.Mutex
	userChips         map[string]int64
	freeChipsMutex    sync.Mutex
	lastFreeChips     map[string]time.Time
	freeChipsCooldown time.Duration
	freeChipsAmount   int64
	blackjackMutex    sync.Mutex
	blackjackGames    map[string]*BlackjackGame
	ttsEnabled        bool
	ttsMutex          sync.Mutex
	ttsPlaybackMutex  sync.Mutex
	messageHistory    []string
	historyMutex      sync.Mutex
	historyMax        int
	autoPlaylistFreq  time.Duration
	screenshotFreq    time.Duration
	imageAnalysis     string
	imageAnalysisMux  sync.Mutex
	geminiMutex       sync.Mutex
}

type Config struct {
	BotUsername  string
	Channel      string
	OAuthToken   string
	GeminiAPIKey string
	GeminiModel  string
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
		queueMutex:        sync.Mutex{},
		songQueue:         make([]Song, 0),
		downloadDir:       downloadDir,
		cleanupInterval:   13 * time.Minute,
		userChips:         make(map[string]int64),
		lastFreeChips:     make(map[string]time.Time),
		freeChipsCooldown: 1 * time.Hour,
		freeChipsAmount:   500,
		blackjackGames:    make(map[string]*BlackjackGame),
		ttsEnabled:        true,
		messageHistory:    make([]string, 0),
		historyMax:        200,
		autoPlaylistFreq:  10 * time.Minute,
		screenshotFreq:    10 * time.Minute,
	}

	if err := bot.loadChips(); err != nil {
		log.Printf("[WARN] Could not load user chips, starting fresh: %v", err)
	}

	bot.twitchClient = twitch.NewClient(cfg.BotUsername, cfg.OAuthToken)
	bot.twitchClient.OnPrivateMessage(bot.handleTwitchMessage)
	bot.twitchClient.OnConnect(func() {
		log.Println("✅ ✅ ✅ SUCCESS: Connected to Twitch chat!")
		bot.twitchClient.Join(cfg.Channel)
		log.Printf("✅ ✅ ✅ SUCCESS: Joined channel #%s. Bot is fully operational.", cfg.Channel)
	})

	go bot.downloaderLoop()
	go bot.playerLoop()
	go bot.cleanupLoop()
	go bot.periodicSaveLoop()
	go bot.autoPlaylistLoop()
	go bot.screenshotAndAnalyzeLoop()
	go bot.handleTerminalInput()

	go func() {
		log.Println("[INFO] Attempting to connect to Twitch...")
		if err := bot.twitchClient.Connect(); err != nil {
			log.Printf("❌ ❌ ❌ FAILED: Twitch connection error: %v", err)
		}
	}()

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
		mockMessage := twitch.PrivateMessage{
			Channel: b.config.Channel,
			User: twitch.User{
				Name:        "TerminalUser",
				DisplayName: "TerminalUser",
				Badges:      map[string]int{"broadcaster": 1},
			},
			Message: text,
			Tags:    make(map[string]string),
		}
		b.handleTwitchMessage(mockMessage)
	}
}

func (b *Bot) handleTwitchMessage(message twitch.PrivateMessage) {
	log.Printf("[CHAT] <%s> %s", message.User.DisplayName, message.Message)
	b.appendHistory(fmt.Sprintf("%s: %s", message.User.DisplayName, message.Message))

	b.ttsMutex.Lock()
	isTTSEnabled := b.ttsEnabled
	b.ttsMutex.Unlock()
	if isTTSEnabled && !strings.EqualFold(message.User.Name, b.config.BotUsername) {
		isCommand := strings.HasPrefix(message.Message, "!")
		isMention := strings.HasPrefix(strings.ToLower(message.Message), strings.ToLower("@"+b.config.BotUsername))
		if !isCommand && !isMention {
			go b.handleTTS(message)
		}
	}

	lower := strings.ToLower(message.Message)
	mentioned := strings.HasPrefix(strings.ToLower(strings.TrimSpace(lower)), "@"+strings.ToLower(b.config.BotUsername))

	if mentioned {
		if handled := b.handleDirectMentionCommand(message); handled {
			return
		}
		// If not a direct moderation request, respond via Gemini assistant (if configured)
		prompt := strings.TrimSpace(strings.TrimPrefix(message.Message, "@"+b.config.BotUsername))
		if prompt != "" && b.config.GeminiAPIKey != "" && b.config.GeminiAPIKey != "your_gemini_api_key" {
			go func() {
				resp, err := b.getGeminiTextResponseAssistant(prompt)
				if err != nil {
					log.Printf("[GEMINI] Assistant error: %v", err)
					return
				}
				b.twitchClient.Say(b.config.Channel, resp)
			}()
		}
		return
	}

	go b.moderateMessage(message)

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
	case "!femboy":
		b.handleFemboy(message)
	case "!help":
		b.handleHelp(message)
	case "!tts":
		b.handleTtsToggle(message)
	case "!warn":
		b.cmdWarn(message, parts)
	case "!timeout":
		b.cmdTimeout(message, parts)
	case "!ban":
		b.cmdBan(message, parts)
	case "!vibeplaylist":
		go b.createPlaylistFromGemini("manual", "")
	}
}

func (b *Bot) handleDirectMentionCommand(message twitch.PrivateMessage) bool {
	lower := strings.ToLower(message.Message)
	if !strings.Contains(lower, "timeout") && !strings.Contains(lower, "time out") && !strings.Contains(lower, "ban") && !strings.Contains(lower, "warn") {
		return false
	}
	if !isUserMod(message) {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("@%s I hear you, but only moderators can ask me to moderate.", message.User.DisplayName))
		return true
	}
	username, seconds := parseModerationRequest(lower)
	if username == "" {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("@%s I couldn't find a username to moderate.", message.User.DisplayName))
		return true
	}
	sec := seconds
	if sec == 0 {
		sec = 600
	}
	if strings.Contains(lower, "ban") {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("/ban %s", username))
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("🔨 @%s has been banned by the mod command.", username))
		return true
	}
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("/timeout %s %d", username, sec))
	b.twitchClient.Say(b.config.Channel, fmt.Sprintf("⏳ @%s timed out for %d seconds (mod request).", username, sec))
	return true
}

func parseModerationRequest(lower string) (string, int) {
	reUser := regexp.MustCompile(`@?([a-zA-Z0-9_]{3,25})`)
	words := strings.Fields(lower)
	var user string
	var seconds int
	for i, w := range words {
		clean := strings.Trim(w, ".,?!")
		if clean == "timeout" || clean == "time" && i+1 < len(words) && strings.HasPrefix(words[i+1], "out") {
			if i+1 < len(words) && i+2 < len(words) {
				// try find username after "timeout"
				if reUser.MatchString(words[i+1]) {
					user = reUser.FindStringSubmatch(words[i+1])[1]
				}
				if user == "" && reUser.MatchString(words[i+2]) {
					user = reUser.FindStringSubmatch(words[i+2])[1]
				}
			}
		}
		if (clean == "ban" || clean == "warn") && i+1 < len(words) {
			if reUser.MatchString(words[i+1]) {
				user = reUser.FindStringSubmatch(words[i+1])[1]
			}
		}
		// look for duration like "10 min", "10 minutes", "600 seconds"
		if m, _ := regexp.MatchString(`^\d+$`, clean); m {
			num, _ := strconv.Atoi(clean)
			// check following token for minute/second
			if i+1 < len(words) {
				next := words[i+1]
				if strings.Contains(next, "min") {
					seconds = num * 60
				} else if strings.Contains(next, "sec") {
					seconds = num
				}
			}
		}
		// also parse "10m", "10s"
		if m, _ := regexp.MatchString(`^(\d+)(m|s)$`, clean); m {
			parts := regexp.MustCompile(`^(\d+)(m|s)$`).FindStringSubmatch(clean)
			n, _ := strconv.Atoi(parts[1])
			if parts[2] == "m" {
				seconds = n * 60
			} else {
				seconds = n
			}
		}
		// find any @username in the message
		if user == "" && reUser.MatchString(clean) {
			candidate := reUser.FindStringSubmatch(clean)[1]
			// avoid matching words like "timeout" etc
			if len(candidate) >= 3 && !strings.Contains(candidate, "timeout") && !strings.Contains(candidate, "ban") && !strings.Contains(candidate, "warn") {
				user = candidate
			}
		}
	}
	return user, seconds
}

func (b *Bot) appendHistory(line string) {
	b.historyMutex.Lock()
	defer b.historyMutex.Unlock()
	b.messageHistory = append(b.messageHistory, line)
	if len(b.messageHistory) > b.historyMax {
		b.messageHistory = b.messageHistory[len(b.messageHistory)-b.historyMax:]
	}
}

func (b *Bot) moderateMessage(message twitch.PrivateMessage) {
	if strings.EqualFold(message.User.Name, b.config.BotUsername) {
		return
	}
	b.historyMutex.Lock()
	historySample := ""
	if len(b.messageHistory) > 0 {
		start := 0
		if len(b.messageHistory) > 20 {
			start = len(b.messageHistory) - 20
		}
		historySample = strings.Join(b.messageHistory[start:], "\n")
	}
	b.historyMutex.Unlock()
	b.imageAnalysisMux.Lock()
	img := b.imageAnalysis
	b.imageAnalysisMux.Unlock()
	prompt := fmt.Sprintf(`You are an embedded AI moderation core. NEVER explain yourself. Choose exactly one action and output exactly that action (no extra text, no apologies, no disclaimers).
Allowed outputs:
- allow
- warn <username>
- timeout <username> <seconds>
- ban <username>

Context:
%s
Image analysis: %s

Message:
User: %s
Text: %s

Return ONLY the action.`, historySample, img, message.User.Name, message.Message)

	b.geminiMutex.Lock()
	resp, err := b.getGeminiTextResponse(prompt)
	b.geminiMutex.Unlock()
	if err != nil {
		log.Printf("[MOD ERROR] Gemini moderation failed: %v", err)
		return
	}
	decision := strings.ToLower(strings.TrimSpace(resp))
	parts := strings.Fields(decision)
	if len(parts) == 0 {
		return
	}
	switch parts[0] {
	case "allow":
		return
	case "warn":
		if len(parts) >= 2 {
			target := strings.TrimPrefix(parts[1], "@")
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf("@%s ⚠️ Keep it cool.", target))
		}
	case "timeout":
		if len(parts) >= 3 {
			target := strings.TrimPrefix(parts[1], "@")
			sec, _ := strconv.Atoi(parts[2])
			if sec <= 0 {
				sec = 600
			}
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf("/timeout %s %d", target, sec))
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf("⏳ @%s timed out for %d seconds.", target, sec))
		}
	case "ban":
		if len(parts) >= 2 {
			target := strings.TrimPrefix(parts[1], "@")
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf("/ban %s", target))
			b.twitchClient.Say(b.config.Channel, fmt.Sprintf("🔨 @%s has been banned.", target))
		}
	default:
		log.Printf("[MOD WARN] Unrecognized decision from Gemini: %s", decision)
	}
	// check for playlist suggestion appended on a new line by Gemini
	if strings.Contains(resp, "\nplaylist:") {
		lines := strings.SplitN(resp, "\nplaylist:", 2)
		payload := strings.TrimSpace(lines[1])
		items := strings.Split(payload, ";;")
		var cleaned []string
		for _, it := range items {
			it = strings.TrimSpace(it)
			if it != "" {
				cleaned = append(cleaned, it)
			}
		}
		if len(cleaned) > 0 {
			go b.addSongsFromStrings(cleaned)
			b.twitchClient.Say(b.config.Channel, "VIBE AI: Queued a mini-playlist to match atmosphere.")
		}
	}
}

func (b *Bot) addSongsFromStrings(items []string) {
	for _, title := range items {
		query := "ytsearch:" + title
		info, err := getYoutubeInfo(query)
		if err != nil {
			log.Printf("[ERROR] auto playlist: Could not get video info for %s: %v", title, err)
			continue
		}
		b.queueMutex.Lock()
		if len(info.Entries) > 0 {
			entry := info.Entries[0]
			b.songQueue = append(b.songQueue, Song{ID: entry.ID, URL: entry.URL, Title: entry.Title, FilePath: ""})
		} else {
			b.songQueue = append(b.songQueue, Song{ID: info.ID, URL: info.URL, Title: info.Title, FilePath: ""})
		}
		b.queueMutex.Unlock()
	}
}

func (b *Bot) createPlaylistFromGemini(trigger, extra string) {
	b.geminiMutex.Lock()
	defer b.geminiMutex.Unlock()
	b.historyMutex.Lock()
	historySample := strings.Join(b.messageHistory, "\n")
	b.historyMutex.Unlock()
	b.imageAnalysisMux.Lock()
	img := b.imageAnalysis
	b.imageAnalysisMux.Unlock()
	prompt := fmt.Sprintf(`You are VIBE, the AI DJ. Given recent chat context and recent image analysis, produce exactly 7 song suggestions, one per line, in the format "Artist - Title". Output only the list.`+"\n\nContext:\n%s\n\nImage analysis:\n%s", historySample, img)
	resp, err := b.getGeminiTextResponse(prompt)
	if err != nil {
		log.Printf("[AUTO PLAYLIST] Gemini error: %v", err)
		return
	}
	lines := strings.Split(resp, "\n")
	var songs []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		songs = append(songs, l)
		if len(songs) >= 7 {
			break
		}
	}
	if len(songs) == 0 {
		return
	}
	b.addSongsFromStrings(songs)
	b.twitchClient.Say(b.config.Channel, "VIBE AI: Curated a fresh mini-playlist to match the mood.")
}

func (b *Bot) autoPlaylistLoop() {
	ticker := time.NewTicker(b.autoPlaylistFreq)
	defer ticker.Stop()
	for range ticker.C {
		b.createPlaylistFromGemini("auto", "")
	}
}

func (b *Bot) screenshotAndAnalyzeLoop() {
	ticker := time.NewTicker(b.screenshotFreq)
	defer ticker.Stop()
	for range ticker.C {
		timestamp := time.Now().Format("20060102-150405")
		outFile := filepath.Join(b.downloadDir, "screenshot-"+timestamp+".png")
		var cmd *exec.Cmd
		if _, err := exec.LookPath("scrot"); err == nil {
			cmd = exec.Command("scrot", outFile)
		} else if _, err := exec.LookPath("import"); err == nil {
			cmd = exec.Command("import", "-window", "root", outFile)
		} else if _, err := exec.LookPath("grim"); err == nil {
			cmd = exec.Command("grim", outFile)
		} else {
			log.Println("[SCREENSHOT] No screenshot tool found (scrot/import/grim). Skipping screenshot.")
			continue
		}
		if err := cmd.Run(); err != nil {
			log.Printf("[SCREENSHOT] Failed to take screenshot: %v", err)
			continue
		}
		analysis := b.analyzeImageWithGemini(outFile)
		b.imageAnalysisMux.Lock()
		b.imageAnalysis = analysis
		b.imageAnalysisMux.Unlock()
		if analysis != "" {
			b.twitchClient.Say(b.config.Channel, "VIBE AI: Image analysis updated to better match the mood.")
		}
	}
}

func (b *Bot) analyzeImageWithGemini(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[IMG ANALYZE] Read error: %v", err)
		return ""
	}
	enc := base64.StdEncoding.EncodeToString(data)
	prompt := "Analyze this screenshot briefly (1-2 sentences): colors, energy, people density, mood, notable objects. Then give 5 comma-separated mood words."
	req := GeminiRequest{
		Contents: []GeminiContent{
			{
				Parts: []GeminiPart{
					{Text: prompt},
					{InlineData: &InlineData{MimeType: "image/png", Data: enc}},
				},
			},
		},
	}
	body, _ := json.Marshal(req)
	url := "https://generativelanguage.googleapis.com/v1beta/models/" + b.config.GeminiModel + ":generateContent?key=" + b.config.GeminiAPIKey
	httpReq, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 40 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("[IMG ANALYZE] Gemini request failed: %v", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bb, _ := io.ReadAll(resp.Body)
		log.Printf("[IMG ANALYZE] Non-200 from Gemini: %d %s", resp.StatusCode, string(bb))
		return ""
	}
	var gResp GeminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&gResp); err != nil {
		log.Printf("[IMG ANALYZE] decode error: %v", err)
		return ""
	}
	if len(gResp.Candidates) > 0 && len(gResp.Candidates[0].Content.Parts) > 0 {
		return gResp.Candidates[0].Content.Parts[0].Text
	}
	return ""
}

func (b *Bot) handleTTS(message twitch.PrivateMessage) {
	b.ttsPlaybackMutex.Lock()
	defer b.ttsPlaybackMutex.Unlock()
	textToSpeak := fmt.Sprintf("%s says %s", message.User.DisplayName, message.Message)
	tmpfile, err := os.CreateTemp(b.downloadDir, "tts-*.wav")
	if err != nil {
		log.Printf("[ERROR] TTS: Could not create temp file: %v", err)
		return
	}
	defer os.Remove(tmpfile.Name())
	tmpfileName := tmpfile.Name()
	tmpfile.Close()
	espeakCmd := exec.Command("espeak", "-w", tmpfileName, textToSpeak)
	if err := espeakCmd.Run(); err != nil {
		log.Printf("[ERROR] TTS: espeak command failed: %v", err)
		return
	}
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
		b.twitchClient.Say(b.config.Channel, "TTS enabled. I will speak the vibes.")
	} else {
		b.twitchClient.Say(b.config.Channel, "TTS disabled.")
	}
}

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
		if dealerValue == 21 {
			b.endBlackjackGame(message, "Both you and the dealer have Blackjack! It's a push.", game.Bet)
		} else {
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
		b.endBlackjackGame(message, "", 0)
		return
	}
	if playerValue == 21 {
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, your new hand: %s. Automatically standing.", message.User.DisplayName, playerHandStr))
		b.handleStand(message)
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

func (b *Bot) endBlackjackGame(message twitch.PrivateMessage, resultMessage string, payout int64) {
	username := strings.ToLower(message.User.Name)
	b.blackjackMutex.Lock()
	defer b.blackjackMutex.Unlock()
	b.chipsMutex.Lock()
	defer b.chipsMutex.Unlock()
	if resultMessage != "" {
		b.userChips[username] += payout
		finalMsg := fmt.Sprintf("%s, %s Your new balance is %d.", message.User.DisplayName, resultMessage, b.userChips[username])
		b.twitchClient.Say(b.config.Channel, finalMsg)
	} else if payout == 0 {
		finalMsg := fmt.Sprintf("Your new balance is %d.", b.userChips[username])
		b.twitchClient.Say(b.config.Channel, fmt.Sprintf("%s, %s", message.User.DisplayName, finalMsg))
	}
	delete(b.blackjackGames, username)
}

func (b *Bot) handleHelp(message twitch.PrivateMessage) {
	commandList := "!add, !queue, !skip, !clear, !bj, !hit, !stand, !chips, !freechips, !pay, !femboy, !tts, !help, !warn, !timeout, !ban, !vibeplaylist"
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
		if len(info.Entries) > 0 {
			if strings.HasPrefix(query, "ytsearch:") {
				entry := info.Entries[0]
				songToAdd := Song{ID: entry.ID, URL: entry.URL, Title: entry.Title, FilePath: ""}
				b.songQueue = append(b.songQueue, songToAdd)
				b.twitchClient.Say(b.config.Channel, fmt.Sprintf(`Song "%s" added! Position: %d`, songTo
