
// Linux: sudo apt install espeak   (oder espeak-ng)
// Windows & macOS: funktioniert sofort ohne Installation

package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func speak(text string) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 300 {
		return
	}
	// Schlechte Zeichen entfernen, die TTS-Probleme macht
	text = strings.ReplaceAll(text, `"`, "")
	text = strings.ReplaceAll(text, `'`, "")
	text = strings.ReplaceAll(text, "`", "")

	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		// Windows hat eingebaute deutsche Stimme (Hedda/Stefan je nach System)
		ps := `Add-Type -AssemblyName System.Speech; ` +
			`$synth = New-Object System.Speech.Synthesis.SpeechSynthesizer; ` +
			`$synth.SelectVoiceByHints('Female','Adult',0,'de-DE'); ` + // versucht deutsche Frauenstimme
			`$synth.Rate = -1; ` + // etwas langsamer, besser verständlich
			`$synth.Speak("` + text + `")`
		cmd = exec.Command("powershell", "-Command", ps)

	case "darwin": // macOS
		// say verwendet automatisch deutsche Stimme, wenn das System auf Deutsch ist
		cmd = exec.Command("say", "-v", "Anna", "-r", "180", text) // Anna = deutsch auf meisten Macs

	case "linux":
		// espeak muss installiert sein
		cmd = exec.Command("espeak", "-v", "de", "-s", "160", text)

	default:
		fmt.Println("TTS nicht unterstützt – Nachricht:", text)
		return
	}

	// non-blocking + überlappend (bei sehr schnellem Chat kann es wild werden)
	go cmd.Run()
}

func main() {
	if len(os.Args) != 2 {
		fmt.Println("Benutzung: go run twitchtts.go <channel>")
		fmt.Println("Beispiel: go run twitchtts.go xqc")
		fmt.Println("          go run twitchtts.go knueppelkalle")
		os.Exit(1)
	}

	channel := strings.ToLower(os.Args[1])

	conn, err := net.DialTimeout("tcp", "irc.chat.twitch.tv:6667", 10*time.Second)
	if err != nil {
		fmt.Println("Kann nicht verbinden:", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Anonymer Nick (justinfan + zufällige Zahl = fast nie Kollision)
	nick := fmt.Sprintf("justinfan%d", time.Now().UnixNano()%999999)
	fmt.Fprintf(conn, "NICK %s\r\n", nick)
	fmt.Fprintf(conn, "JOIN #%s\r\n", channel)

	fmt.Printf("Verbunden mit #%s – warte auf Chatnachrichten...\n", channel)

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()

		// PING → PONG
		if strings.HasPrefix(line, "PING") {
			conn.Write([]byte("PONG :tmi.twitch.tv\r\n"))
			continue
		}

		// PRIVMSG parsen
		if strings.Contains(line, "PRIVMSG #"+channel+" :") {
			// User extrahieren (vor dem ersten !)
			userStart := strings.Index(line, ":") + 1
			userEnd := strings.Index(line, "!")
			if userStart < 0 || userEnd < 0 || userEnd < userStart {
				continue
			}
			user := line[userStart:userEnd]

			// Nachricht extrahieren (alles nach dem letzten :)
			msgStart := strings.LastIndex(line, ":")
			if msgStart == -1 {
				continue
			}
			message := line[msgStart+1:]

			output := user + " sagt: " + message
			fmt.Println(output)
			speak(output)
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Println("Verbindung verloren:", err)
	}
}
