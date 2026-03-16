package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Whitfrost21/FlareSpark/internal/filereader"
	"github.com/Whitfrost21/FlareSpark/internal/filewriter"
	"github.com/Whitfrost21/FlareSpark/internal/groq"
	"github.com/Whitfrost21/FlareSpark/internal/handler"
	"github.com/Whitfrost21/FlareSpark/internal/rpc"
	"github.com/Whitfrost21/FlareSpark/internal/session"
	"github.com/Whitfrost21/FlareSpark/internal/transport"
)

func main() {
	apiKey := resolveAPIKey()
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "[flarespark] ERROR: GROQ_API_KEY not found.")
		fmt.Fprintln(os.Stderr, "[flarespark] Set it in one of:")
		fmt.Fprintln(os.Stderr, "  1. ~/.config/flarespark/config  →  GROQ_API_KEY=gsk_...")
		fmt.Fprintln(os.Stderr, "  2. ~/.env or ~/.env.local        →  GROQ_API_KEY=gsk_...")
		fmt.Fprintln(os.Stderr, "  3. Zed settings.json env block   →  \"GROQ_API_KEY\": \"gsk_...\"")
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "[flarespark] started, default model:", groq.DefaultModel)

	transport.Init()

	fr := filereader.New()
	fw := filewriter.New()

	h := handler.New(
		session.NewStore(),
		&groq.Client{APIKey: apiKey},
		fr,
		fw,
	)

	scanner := transport.Scanner()
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		fmt.Fprintln(os.Stderr, "[flarespark] recv:", line)

		var msg rpc.Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			fmt.Fprintln(os.Stderr, "[flarespark] parse error:", err)
			continue
		}
		msg.Raw = []byte(line)

		// Route responses to our outgoing requests (no Method = response).
		if msg.Method == "" && msg.ID != nil {
			var rawID string
			json.Unmarshal(*msg.ID, &rawID)

			switch {
			case filewriter.IsWriteResponse(rawID):
				id, errMsg := filewriter.ParseResponse(msg)
				fw.Deliver(id, errMsg)
			default:
				id, content, errMsg := filereader.ParseResponse(msg)
				fr.Deliver(id, content, errMsg)
			}
			continue
		}

		h.Dispatch(msg)
	}
}

// resolveAPIKey tries multiple sources in priority order:
//  1. GROQ_API_KEY already in environment (e.g. set via Zed settings.json)
//  2. ~/.config/flarespark/config  (KEY=value format, one per line)
//  3. ~/.env and ~/.env.local      (standard dotenv files)
func resolveAPIKey() string {
	// 1. Already in env (Zed settings.json env block, or shell did export it)
	if v := os.Getenv("GROQ_API_KEY"); v != "" {
		fmt.Fprintln(os.Stderr, "[flarespark] API key loaded from environment")
		return v
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	// 2. Dedicated config file
	candidates := []string{
		filepath.Join(home, ".config", "flarespark", "config"),
		filepath.Join(home, ".env.local"),
		filepath.Join(home, ".env"),
	}

	for _, path := range candidates {
		if key := readKeyFromFile(path); key != "" {
			fmt.Fprintf(os.Stderr, "[flarespark] API key loaded from %s\n", path)
			return key
		}
	}

	return ""
}

// readKeyFromFile scans a KEY=value file for GROQ_API_KEY.
func readKeyFromFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		val, found := strings.CutPrefix(line, "GROQ_API_KEY=")
		if found {
			val = strings.Trim(val, `"'`) // strip optional quotes
			return val
		}
	}
	return ""
}
