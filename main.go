// geostats.go
// build: go1.23
// ------------------------------------------------------------
// Features
//   - Collects Standard (V3) and Duels games, de‑duplicates & stores rounds in SQLite (modernc.org/sqlite)
//   - Reverse‑geocodes lat/lng to ISO country codes via countries.json (GeoJSON)
//   - Exposes REST API:
//     /api/update_ncfa?token=…        – update cookie
//     /api/collect_now                – pull fresh feed & persist
//     /api/summary?type=standard|duels&move=Moving|NoMove|NMPZ (all default) – aggregated stats
//     /api/games?type=standard|duels&limit=30       – recent game list
//     /api/game?id=<game_id>          – full round breakdown
//   - Serves HTML UI (index -> tabs for Singleplayer/Duels, list of games, charts for overall stats & per‑game details) using Chart.js (CDN)
//   - Everything pure Go; no cgo.
//
// ------------------------------------------------------------
package main

import (
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go driver

	"github.com/kardianos/service"
	"github.com/paulmach/orb/geojson"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

const currentVersion = "0.5.5"

//go:embed countries.json templates/*
var embeddedFS embed.FS

// Global template variable
var templates *template.Template

// Configuration structure
type Config struct {
	NCFA       string `yaml:"ncfa"`
	ListenIP   string `yaml:"listen_ip"`
	Port       int    `yaml:"port"`
	Debug      bool   `yaml:"debug,omitempty"`
	LogDir     string `yaml:"log_directory,omitempty"`
	IsPublic   bool   `yaml:"is_public"`
	PrivateKey string `yaml:"private_key"`
}

// Global configuration
var (
	config    *Config
	configDir string
)

// Global country coder for name lookups
var countryCoder *CountryCoder

// Service related globals
var (
	httpServer *http.Server
	logger     service.Logger
)

// GeoStatsr service struct
type geoStatsrService struct{}

func (s *geoStatsrService) Start(svc service.Service) error {
	if logger != nil {
		logger.Info("Starting GeoStatsr service")
	}
	go s.run()
	return nil
}

func (s *geoStatsrService) Stop(svc service.Service) error {
	if logger != nil {
		logger.Info("Stopping GeoStatsr service")
	}
	if httpServer != nil {
		return httpServer.Close()
	}
	return nil
}

func (s *geoStatsrService) run() {
	// Initialize database and templates
	initDB()
	initTemplates()
	countryCoder = NewCountryCoder(configDir) // Initialize global country coder

	// Setup HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/api/update_ncfa", apiUpdateCookie)
	mux.HandleFunc("/api/collect_now", apiCollectNow)
	mux.HandleFunc("/api/summary", apiSummary)
	mux.HandleFunc("/api/games", apiGames)
	mux.HandleFunc("/api/game", apiGame)
	mux.HandleFunc("/api/game_map_data", apiGameMapData)
	mux.HandleFunc("/api/country_stats", apiCountryStats)
	mux.HandleFunc("/api/chart_data", apiChartData)
	mux.HandleFunc("/api/map_data", apiMapData)
	mux.HandleFunc("/api/countries_geojson", apiCountriesGeoJSON)
	mux.HandleFunc("/api/confused_countries", apiConfusedCountries)
	// Country-specific routes
	mux.HandleFunc("/api/country/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/summary") {
			apiCountrySummary(w, r)
		} else if strings.HasSuffix(path, "/confused") {
			apiCountryConfused(w, r)
		} else if strings.HasSuffix(path, "/rounds") {
			apiCountryRounds(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/country/", uiCountry)
	// Opponent UI route
	mux.HandleFunc("/opponent/", uiOpponent)
	// Static file handler with proper MIME types
	staticDir := filepath.Join(configDir, "static")
	fs := http.FileServer(http.Dir(staticDir))
	mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
		// Set proper MIME types based on file extension
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, ".css"):
			w.Header().Set("Content-Type", "text/css")
		case strings.HasSuffix(path, ".js"):
			w.Header().Set("Content-Type", "text/javascript")
		case strings.HasSuffix(path, ".json"):
			w.Header().Set("Content-Type", "application/json")
		case strings.HasSuffix(path, ".png"):
			w.Header().Set("Content-Type", "image/png")
		case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
			w.Header().Set("Content-Type", "image/jpeg")
		case strings.HasSuffix(path, ".gif"):
			w.Header().Set("Content-Type", "image/gif")
		case strings.HasSuffix(path, ".svg"):
			w.Header().Set("Content-Type", "image/svg+xml")
		case strings.HasSuffix(path, ".webp"):
			w.Header().Set("Content-Type", "image/webp")
		case strings.HasSuffix(path, ".woff2"):
			w.Header().Set("Content-Type", "font/woff2")
		case strings.HasSuffix(path, ".woff"):
			w.Header().Set("Content-Type", "font/woff")
		case strings.HasSuffix(path, ".ico"):
			w.Header().Set("Content-Type", "image/x-icon")
		}

		// Remove the /static/ prefix and serve the file
		http.StripPrefix("/static/", fs).ServeHTTP(w, r)
	})
	mux.HandleFunc("/stats_row", uiStatsRow)
	mux.HandleFunc("/", uiIndex)

	// Opponent API endpoints
	mux.HandleFunc("/api/opponent/", func(w http.ResponseWriter, r *http.Request) {
		// /api/opponent/{id}/summary, /matches, /score-comparison, /countries, /performance
		path := r.URL.Path
		parts := strings.Split(path, "/")
		if len(parts) < 4 {
			http.NotFound(w, r)
			return
		}
		opponentId := parts[3]
		if len(parts) == 5 {
			switch parts[4] {
			case "summary":
				apiOpponentSummary(w, r, opponentId)
				return
			case "matches":
				apiOpponentMatches(w, r, opponentId)
				return
			case "score-comparison":
				apiOpponentScoreComparison(w, r, opponentId)
				return
			case "countries":
				apiOpponentCountries(w, r, opponentId)
				return
			case "performance":
				apiOpponentPerformance(w, r, opponentId)
				return
			}
		}
		http.NotFound(w, r)
	})

	listenAddr := fmt.Sprintf("%s:%d", config.ListenIP, config.Port)
	httpServer = &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	// Start periodic tasks
	startPeriodicTasks()

	if logger != nil {
		logger.Infof("Server starting on %s – open http://localhost:%d/", listenAddr, config.Port)
		if config.IsPublic {
			logger.Infof("Running in PUBLIC mode - API updates require private key: %s", config.PrivateKey)
		} else {
			logger.Info("Running in PRIVATE mode - API updates do not require authentication")
		}
		if config.NCFA == "" {
			logger.Warning("NCFA cookie not set. Use /api/update_ncfa?token=YOUR_COOKIE to set it.")
		}
	} else {
		log.Printf("Server starting on %s – open http://localhost:%d/", listenAddr, config.Port)
		if config.IsPublic {
			log.Printf("Running in PUBLIC mode - API updates require private key: %s", config.PrivateKey)
		} else {
			log.Printf("Running in PRIVATE mode - API updates do not require authentication")
		}
		if config.NCFA == "" {
			log.Printf("WARNING: NCFA cookie not set. Use /api/update_ncfa?token=YOUR_COOKIE to set it.")
		}
	}

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		if logger != nil {
			logger.Errorf("Server error: %v", err)
		} else {
			log.Printf("Server error: %v", err)
		}
	}
}

// ------------------------------------------------------------
// Configuration management

func generatePrivateKey() string {
	bytes := make([]byte, 16) // 32 hex characters = 16 bytes
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func loadConfig() (*Config, error) {
	// Ensure configDir is absolute path
	var err error
	configDir, err = filepath.Abs(configDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve config directory path: %v", err)
	}

	configPath := filepath.Join(configDir, "geostatsr.yaml")

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Create default config
		defaultConfig := &Config{
			NCFA:       "",
			ListenIP:   "0.0.0.0",
			Port:       62826,
			IsPublic:   false,
			PrivateKey: generatePrivateKey(),
		}

		// Check if config directory is writable
		if err := os.MkdirAll(configDir, 0755); err != nil {
			return nil, fmt.Errorf("cannot create config directory %s: %v", configDir, err)
		}

		// Test write permissions
		testFile := filepath.Join(configDir, ".write_test")
		if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
			return nil, fmt.Errorf("config directory %s is not writable: %v", configDir, err)
		}
		os.Remove(testFile)

		// Save default config
		if err := saveConfig(defaultConfig); err != nil {
			return nil, fmt.Errorf("failed to create default config: %v", err)
		}

		log.Printf("Created default configuration at %s", configPath)
		return defaultConfig, nil
	}

	// Load existing config
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	// Set defaults for missing values
	if cfg.ListenIP == "" {
		cfg.ListenIP = "0.0.0.0"
	}
	if cfg.Port == 0 {
		cfg.Port = 62826
	}
	if cfg.PrivateKey == "" {
		cfg.PrivateKey = generatePrivateKey()
		saveConfig(&cfg) // Save the generated key
	}

	return &cfg, nil
}

func saveConfig(cfg *Config) error {
	configPath := filepath.Join(configDir, "geostatsr.yaml")

	// Add comments to the YAML
	configContent := `# GeoStatsr Configuration File
#
# ncfa: Your GeoGuessr NCFA cookie value (leave empty initially, update via API)
ncfa: "` + cfg.NCFA + `"

# Server settings
listen_ip: "` + cfg.ListenIP + `"   # IP to bind to (0.0.0.0 for all interfaces)
port: ` + fmt.Sprintf("%d", cfg.Port) + `                # Port to listen on

# Optional settings (uncomment to enable)
# debug: true                        # Enable debug logging
# log_directory: "/path/to/logs"     # Directory for log files when debug is enabled

# Security settings
is_public: ` + fmt.Sprintf("%t", cfg.IsPublic) + `               # If true, requires private key for API updates
private_key: "` + cfg.PrivateKey + `"  # Private key for API access (auto-generated)
`

	return os.WriteFile(configPath, []byte(configContent), 0644)
}

func debugLog(format string, args ...interface{}) {
	if config.Debug {
		if config.LogDir != "" {
			// Log to file if directory is specified
			logFile := filepath.Join(config.LogDir, "debug.log")
			if f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
				defer f.Close()
				fmt.Fprintf(f, "[DEBUG] %s: %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
			}
		}
		log.Printf("[DEBUG] "+format, args...)
	}
}

// ------------------------------------------------------------
// runtime cookie access

func currentNCFA() string {
	return config.NCFA
}

// ------------------------------------------------------------
// HTTP client with cookie on every request
func apiClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse("https://www.geoguessr.com")
	jar.SetCookies(u, []*http.Cookie{{Name: "_ncfa", Value: currentNCFA()}})
	return &http.Client{Jar: jar, Timeout: 25 * time.Second}
}

// ------------------------------------------------------------
// country lookup via GeoJSON polygons - DEPRECATED, using CountryCoder now

// Legacy country index kept for compatibility during transition
type countryIndex struct{ features []*geojson.Feature }

func loadCountries() *countryIndex {
	data, err := embeddedFS.ReadFile("countries.json")
	if err != nil {
		log.Fatalf("countries.json missing: %v", err)
	}
	coll, err := geojson.UnmarshalFeatureCollection(data)
	if err != nil {
		log.Fatalf("bad GeoJSON: %v", err)
	}
	return &countryIndex{features: coll.Features}
}

// Legacy methods - now using CountryCoder
func (ci *countryIndex) code(lat, lng float64) string {
	return countryCoder.CodeByLocation(lat, lng)
}

func (ci *countryIndex) name(countryCode string) string {
	return countryCoder.NameEnByCode(countryCode)
}

// ------------------------------------------------------------
// SQLite initialisation / helpers
var db *sql.DB

func initDB() {
	var err error
	dbPath := filepath.Join(configDir, "geostats.db")
	db, err = sql.Open("sqlite", fmt.Sprintf("file:%s?_busy_timeout=30000&_fk=1", dbPath))
	if err != nil {
		log.Fatal(err)
	}
	schema := `
CREATE TABLE IF NOT EXISTS games(
    id TEXT PRIMARY KEY,
    game_type TEXT,           -- standard | duels
    movement TEXT,            -- Moving | NoMove | NMPZ
    created TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    map_name TEXT,            -- name of the map played
    game_date TIMESTAMP,      -- actual game play date from API
    -- Duels result fields
    is_draw BOOLEAN,          -- whether the duel ended in a draw
    winning_team_id TEXT,     -- ID of the winning team
    winner_style TEXT,        -- style of victory (e.g., "FlawlessVictory")
    -- Opponent tracking fields for duels
    opponent_id TEXT,         -- opponent player ID
    opponent_nick TEXT,       -- opponent nickname
    player_team_id TEXT       -- player's team ID
);
CREATE TABLE IF NOT EXISTS rounds(
    game_id TEXT,
    round_no INTEGER,
    player_score REAL,
    opponent_score REAL,
    player_lat REAL, player_lng REAL,
    opponent_lat REAL, opponent_lng REAL,
    player_dist REAL, opponent_dist REAL,
    country_code TEXT,
    -- New fields for actual location and metadata
    actual_lat REAL, actual_lng REAL,
    actual_country_code TEXT,
    round_multiplier REAL DEFAULT 1,
    player_health_before INTEGER,
    player_health_after INTEGER,
    opponent_health_before INTEGER,
    opponent_health_after INTEGER,
    round_start_time INTEGER,
    round_end_time INTEGER,
    -- Fields for singleplayer games
    round_time INTEGER,
    steps_count INTEGER,
    timed_out BOOLEAN,
    score_percentage REAL,
    PRIMARY KEY(game_id, round_no),
    FOREIGN KEY(game_id) REFERENCES games(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS user_metadata(
    key TEXT PRIMARY KEY,
    value TEXT,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS br_rank(
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    level INTEGER,
    division INTEGER,
    recorded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS competition_medals(
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    bronze INTEGER DEFAULT 0,
    silver INTEGER DEFAULT 0,
    gold INTEGER DEFAULT 0,
    platinum INTEGER DEFAULT 0,
    recorded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS competitive_rank(
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    elo INTEGER DEFAULT 0,
    rating INTEGER DEFAULT 0,
    last_rating_change INTEGER DEFAULT 0,
    division_type INTEGER,
    division_start_rating INTEGER,
    division_end_rating INTEGER,
    on_leaderboard BOOLEAN DEFAULT FALSE,
    recorded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
`
	if _, err = db.Exec(schema); err != nil {
		log.Fatal(err)
	}
}

// Initialize templates from embedded files or external directory
func initTemplates() {
	// Check if templates exist in config directory
	templatesDir := filepath.Join(configDir, "templates")
	if _, err := os.Stat(templatesDir); err == nil {
		// Use external templates from config directory
		var parseErr error
		templates, parseErr = template.ParseGlob(filepath.Join(templatesDir, "*.html"))
		if parseErr != nil {
			log.Printf("Warning: Failed to parse external templates from %s, falling back to embedded: %v", templatesDir, parseErr)
			// Fall back to embedded templates
			templates, parseErr = template.ParseFS(embeddedFS, "templates/*.html")
			if parseErr != nil {
				log.Fatalf("Failed to parse embedded templates: %v", parseErr)
			}
		} else {
			log.Printf("Using external templates from %s", templatesDir)
		}
	} else {
		// Use embedded templates
		var parseErr error
		templates, parseErr = template.ParseFS(embeddedFS, "templates/*.html")
		if parseErr != nil {
			log.Fatalf("Failed to parse embedded templates: %v", parseErr)
		}
		log.Printf("Using embedded templates")
	}
}

// ------------------------------------------------------------
// Regex helpers for feed & HTML parsing
var (
	tokRE      = regexp.MustCompile(`"(?:gameToken|challengeToken)":"([^"]+)"`)
	duelRE     = regexp.MustCompile(`"gameId":"([^"]+)"`)
	nextDataRE = regexp.MustCompile(`<script id="__NEXT_DATA__"[^>]*>(.+?)</script>`)
)

// ------------------------------------------------------------
// minimal structs for API JSON parsing (trimmed)

type v3Game struct {
	ForbidMoving, ForbidZooming, ForbidRotating bool
	MapName                                     string `json:"mapName"`
	Player                                      struct {
		TotalScore struct{ Amount string } `json:"totalScore"`
		Guesses    []struct {
			RoundScoreInPoints     float64                                  `json:"roundScoreInPoints"`
			RoundScoreInPercentage float64                                  `json:"roundScoreInPercentage"`
			Distance               struct{ Meters struct{ Amount string } } `json:"distance"`
			Lat, Lng               float64
			TimedOut               bool `json:"timedOut"`
			TimedOutWithGuess      bool `json:"timedOutWithGuess"`
			StepsCount             int  `json:"stepsCount"`
			Time                   int  `json:"time"`
		}
	}
	Rounds []struct {
		StreakLocationCode string `json:"streakLocationCode"`
		Lat, Lng           float64
		StartTime          string `json:"startTime"`
	}
}

type v4Summary struct {
	Props struct {
		PageProps struct {
			UserId string
			Game   struct {
				Options struct {
					MovementOptions struct {
						ForbidMoving, ForbidZooming, ForbidRotating bool
					}
				}
				Teams []struct {
					Id      string `json:"id"`
					Name    string `json:"name"`
					Players []struct {
						PlayerId string `json:"playerId"`
						Nick     string `json:"nick"`
						Guesses  []struct {
							RoundNumber int     `json:"roundNumber"`
							Score       float64 `json:"score"`
							Lat         float64 `json:"lat"`
							Lng         float64 `json:"lng"`
							Distance    float64 `json:"distance"`
						}
					}
					RoundResults []struct {
						RoundNumber  int `json:"roundNumber"`
						Score        int `json:"score"`
						HealthBefore int `json:"healthBefore"`
						HealthAfter  int `json:"healthAfter"`
					} `json:"roundResults"`
				}
				Rounds []struct {
					RoundNumber int `json:"roundNumber"`
					Panorama    struct {
						Lat         float64 `json:"lat"`
						Lng         float64 `json:"lng"`
						CountryCode string  `json:"countryCode"`
					} `json:"panorama"`
					Multiplier float64 `json:"multiplier"`
					StartTime  int64   `json:"startTime"`
					EndTime    int64   `json:"endTime"`
				} `json:"rounds"`
				Result struct {
					IsDraw        bool   `json:"isDraw"`
					WinningTeamId string `json:"winningTeamId"`
					WinnerStyle   string `json:"winnerStyle"`
				} `json:"result"`
			}
		}
	}
}

// ------------------------------------------------------------
// movement detection helper
func mode(noMove, noZoom, noRot bool) string {
	switch {
	case noMove && noZoom && noRot:
		return "NMPZ"
	case noMove:
		return "NoMove"
	default:
		return "Moving"
	}
}

// ------------------------------------------------------------
// Feed crawler
const (
	baseV3 = "https://www.geoguessr.com/api/v3"
	baseV4 = "https://www.geoguessr.com/api/v4"
)

func pullFeed() (std []string, duels []string) {
	client := apiClient()
	var page string
	pageCount := 0

	debugLog("Starting feed pull...")

	for {
		pageCount++
		u := baseV4 + "/feed/private"
		if page != "" {
			u += "?paginationToken=" + page
		}

		debugLog("Page %d: Fetching %s", pageCount, u)

		resp, err := client.Get(u)
		if err != nil {
			debugLog("Page %d: HTTP error: %v", pageCount, err)
			break
		}

		if resp.StatusCode != 200 {
			debugLog("Page %d: HTTP status %d", pageCount, resp.StatusCode)
			// Read and log the response body for debugging
			if body, err := io.ReadAll(resp.Body); err == nil {
				debugLog("Page %d: Response body: %s", pageCount, string(body)[:min(500, len(body))])
			}
			resp.Body.Close()
			break
		}

		// Read the response body for debugging before JSON decoding
		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			debugLog("Page %d: Failed to read response body: %v", pageCount, err)
			break
		}

		// Log a sample of the raw response for debugging
		if pageCount <= 2 {
			debugLog("Page %d: Raw response sample: %s", pageCount, string(bodyBytes)[:min(1000, len(bodyBytes))])
		}

		var body struct {
			Entries         []struct{ Payload string }
			PaginationToken string
		}

		if err = json.Unmarshal(bodyBytes, &body); err != nil {
			debugLog("Page %d: JSON decode error: %v", pageCount, err)
			debugLog("Page %d: Raw response causing error: %s", pageCount, string(bodyBytes)[:min(1000, len(bodyBytes))])
			break
		}

		debugLog("Page %d: Got %d entries, PaginationToken: %q", pageCount, len(body.Entries), body.PaginationToken)

		// Track games found on this page
		pageStd := 0
		pageDuels := 0

		for i, e := range body.Entries {
			// Log first few payloads to see what we're working with
			if i < 3 {
				debugLog("Page %d Entry %d: %s", pageCount, i, e.Payload[:min(200, len(e.Payload))])
			}

			// Extract games from this entry
			entryStd, entryDuels := extractGamesFromPayload(e.Payload, pageCount, i)
			std = append(std, entryStd...)
			duels = append(duels, entryDuels...)
			pageStd += len(entryStd)
			pageDuels += len(entryDuels)
		}

		debugLog("Page %d results: %d Standard, %d Duels games found", pageCount, pageStd, pageDuels)
		debugLog("Total so far: %d Standard, %d Duels games", len(std), len(duels))

		// Check if we have more pages
		if body.PaginationToken == "" {
			debugLog("No more pages - PaginationToken is empty")
			break
		}

		// Prevent infinite loops
		if pageCount >= 50 {
			debugLog("Stopping at page %d to prevent infinite loop", pageCount)
			break
		}

		page = body.PaginationToken
		debugLog("Page %d: Setting next page token: %s", pageCount, page[:min(50, len(page))])

		// Add a small delay to be respectful to the API
		time.Sleep(200 * time.Millisecond)
	}

	debugLog("Feed pull complete: %d pages processed, %d Standard games, %d Duels games", pageCount, len(std), len(duels))
	return
}

// extractGamesFromPayload extracts all games from a single payload entry
func extractGamesFromPayload(payload string, pageNum, entryNum int) (std []string, duels []string) {
	// Check if payload contains JSON array of games
	if strings.HasPrefix(payload, "[") {
		// Parse as JSON array
		var payloadArray []struct {
			Payload struct {
				GameId         string `json:"gameId"`
				GameToken      string `json:"gameToken"`
				ChallengeToken string `json:"challengeToken"`
				GameMode       string `json:"gameMode"`
			} `json:"payload"`
		}

		if err := json.Unmarshal([]byte(payload), &payloadArray); err == nil {
			for _, item := range payloadArray {
				switch item.Payload.GameMode {
				case "Standard":
					token := item.Payload.GameToken
					if token == "" {
						token = item.Payload.ChallengeToken
					}
					if token != "" {
						std = append(std, token)
						debugLog("Page %d Entry %d: Found Standard game in array: %s", pageNum, entryNum, token)
					}
				case "Duels":
					if item.Payload.GameId != "" {
						duels = append(duels, item.Payload.GameId)
						debugLog("Page %d Entry %d: Found Duels game in array: %s", pageNum, entryNum, item.Payload.GameId)
					}
				}
			}
			return
		}
	}

	// Fallback to direct object parsing if not an array
	var payloadObj struct {
		GameId         string `json:"gameId"`
		GameToken      string `json:"gameToken"`
		ChallengeToken string `json:"challengeToken"`
		GameMode       string `json:"gameMode"`
	}

	if err := json.Unmarshal([]byte(payload), &payloadObj); err == nil {
		switch payloadObj.GameMode {
		case "Standard":
			token := payloadObj.GameToken
			if token == "" {
				token = payloadObj.ChallengeToken
			}
			if token != "" {
				std = append(std, token)
				debugLog("Page %d Entry %d: Found Standard game: %s", pageNum, entryNum, token)
			}
		case "Duels":
			if payloadObj.GameId != "" {
				duels = append(duels, payloadObj.GameId)
				debugLog("Page %d Entry %d: Found Duels game: %s", pageNum, entryNum, payloadObj.GameId)
			}
		}
		return
	}

	// Final fallback to regex-based parsing for malformed JSON
	if strings.Contains(payload, `"gameMode":"Standard"`) {
		if m := tokRE.FindAllStringSubmatch(payload, -1); len(m) > 0 {
			for _, match := range m {
				if len(match) == 2 {
					std = append(std, match[1])
					debugLog("Page %d Entry %d: Found Standard game via regex: %s", pageNum, entryNum, match[1])
				}
			}
		}
	}

	if strings.Contains(payload, `"gameMode":"Duels"`) {
		if m := duelRE.FindAllStringSubmatch(payload, -1); len(m) > 0 {
			for _, match := range m {
				if len(match) == 2 {
					duels = append(duels, match[1])
					debugLog("Page %d Entry %d: Found Duels game via regex: %s", pageNum, entryNum, match[1])
				}
			}
		}
	}

	return
}

// Helper function for min
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Helper function to safely convert string to float64
func safeStringToFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// Calculate distance between two coordinates using Haversine formula
// Returns distance in kilometers
func haversineDistance(lat1, lng1, lat2, lng2 float64) float64 {
	const R = 6371 // Earth's radius in kilometers

	// Convert degrees to radians
	dLat := (lat2 - lat1) * math.Pi / 180
	dLng := (lng2 - lng1) * math.Pi / 180
	lat1Rad := lat1 * math.Pi / 180
	lat2Rad := lat2 * math.Pi / 180

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return R * c
}

// ------------------------------------------------------------
// persistence helpers

func insertGame(id, typ, mov string, gameDate ...string) {
	mapName := ""
	var isDraw *bool
	var winningTeamId *string
	var winnerStyle *string
	var opponentId *string
	var opponentNick *string
	var playerTeamId *string

	if len(gameDate) > 1 && gameDate[1] != "" {
		mapName = gameDate[1]
	}

	// Handle duels result data (gameDate[2], gameDate[3], gameDate[4])
	if len(gameDate) > 2 && gameDate[2] != "" {
		// Parse isDraw
		if gameDate[2] == "true" {
			isDraw = &[]bool{true}[0]
		} else if gameDate[2] == "false" {
			isDraw = &[]bool{false}[0]
		}
	}
	if len(gameDate) > 3 && gameDate[3] != "" {
		winningTeamId = &gameDate[3]
	}
	if len(gameDate) > 4 && gameDate[4] != "" {
		winnerStyle = &gameDate[4]
	}
	// Handle opponent data (gameDate[5], gameDate[6], gameDate[7])
	if len(gameDate) > 5 && gameDate[5] != "" {
		opponentId = &gameDate[5]
	}
	if len(gameDate) > 6 && gameDate[6] != "" {
		opponentNick = &gameDate[6]
	}
	if len(gameDate) > 7 && gameDate[7] != "" {
		playerTeamId = &gameDate[7]
	}

	var err error
	if len(gameDate) > 0 && gameDate[0] != "" {
		normalizedDate := normalizeGameDate(gameDate[0])
		if mapName != "" && isDraw == nil {
			// Standard game with map name
			_, err = db.Exec(`INSERT OR IGNORE INTO games(id,game_type,movement,game_date,map_name) VALUES(?,?,?,?,?)`, id, typ, mov, normalizedDate, mapName)
		} else if mapName != "" && isDraw != nil {
			// Duels game with map name and result
			_, err = db.Exec(`INSERT OR IGNORE INTO games(id,game_type,movement,game_date,map_name,is_draw,winning_team_id,winner_style,opponent_id,opponent_nick,player_team_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				id, typ, mov, normalizedDate, mapName, isDraw, winningTeamId, winnerStyle, opponentId, opponentNick, playerTeamId)
		} else if isDraw != nil {
			// Duels game with result but no map name
			_, err = db.Exec(`INSERT OR IGNORE INTO games(id,game_type,movement,game_date,is_draw,winning_team_id,winner_style,opponent_id,opponent_nick,player_team_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
				id, typ, mov, normalizedDate, isDraw, winningTeamId, winnerStyle, opponentId, opponentNick, playerTeamId)
		} else {
			// Standard game without map name
			_, err = db.Exec(`INSERT OR IGNORE INTO games(id,game_type,movement,game_date) VALUES(?,?,?,?)`, id, typ, mov, normalizedDate)
		}
	} else {
		if mapName != "" && isDraw == nil {
			// Standard game with map name, no date
			_, err = db.Exec(`INSERT OR IGNORE INTO games(id,game_type,movement,map_name) VALUES(?,?,?,?)`, id, typ, mov, mapName)
		} else if mapName != "" && isDraw != nil {
			// Duels game with map name and result, no date
			_, err = db.Exec(`INSERT OR IGNORE INTO games(id,game_type,movement,map_name,is_draw,winning_team_id,winner_style,opponent_id,opponent_nick,player_team_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
				id, typ, mov, mapName, isDraw, winningTeamId, winnerStyle, opponentId, opponentNick, playerTeamId)
		} else if isDraw != nil {
			// Duels game with result but no map name or date
			_, err = db.Exec(`INSERT OR IGNORE INTO games(id,game_type,movement,is_draw,winning_team_id,winner_style,opponent_id,opponent_nick,player_team_id) VALUES(?,?,?,?,?,?,?,?,?)`,
				id, typ, mov, isDraw, winningTeamId, winnerStyle, opponentId, opponentNick, playerTeamId)
		} else {
			// Standard game without map name or date
			_, err = db.Exec(`INSERT OR IGNORE INTO games(id,game_type,movement) VALUES(?,?,?)`, id, typ, mov)
		}
	}

	if err != nil {
		debugLog("insertGame error: %v", err)
	}
}

// --- single games
func storeStandard(id string, ci *countryIndex) {
	debugLog("storeStandard: Processing game %s", id)
	if rowExists(`SELECT 1 FROM rounds WHERE game_id=? LIMIT 1`, id) {
		debugLog("storeStandard: Game %s already exists, skipping", id)
		return
	}

	url := baseV3 + "/games/" + id
	debugLog("storeStandard: Fetching %s", url)
	resp, err := apiClient().Get(url)
	if err != nil {
		debugLog("storeStandard: v3 fetch error for %s: %v", id, err)
		return
	}

	if resp.StatusCode != 200 {
		debugLog("storeStandard: HTTP %d for game %s", resp.StatusCode, id)
		resp.Body.Close()
		return
	}

	var g v3Game
	if err = json.NewDecoder(resp.Body).Decode(&g); err != nil {
		debugLog("storeStandard: JSON decode error for %s: %v", id, err)
		resp.Body.Close()
		return
	}
	resp.Body.Close()

	debugLog("storeStandard: Successfully parsed game %s, %d guesses", id, len(g.Player.Guesses))
	m := mode(g.ForbidMoving, g.ForbidZooming, g.ForbidRotating)
	debugLog("storeStandard: Movement mode: %s", m)

	// Extract game date from first round's start time if available
	var gameDate string
	if len(g.Rounds) > 0 && g.Rounds[0].StartTime != "" {
		gameDate = g.Rounds[0].StartTime
	}
	insertGame(id, "standard", m, gameDate, g.MapName)

	tx, _ := db.Begin()
	stmt, _ := tx.Prepare(`INSERT OR IGNORE INTO rounds(
		game_id, round_no, player_score,
		player_lat, player_lng, player_dist, country_code,
		actual_lat, actual_lng, actual_country_code,
		round_time, steps_count, timed_out, score_percentage
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	debugLog("storeStandard: Inserting %d rounds for game %s", len(g.Player.Guesses), id)
	for i, guess := range g.Player.Guesses {
		// Country code from where the player guessed (based on their guess coordinates)
		guessedCC := ci.code(guess.Lat, guess.Lng)

		// Actual location data from the round
		actualCC := g.Rounds[i].StreakLocationCode
		actualLat := g.Rounds[i].Lat
		actualLng := g.Rounds[i].Lng

		// If we don't have the actual country code, derive it from actual coordinates
		if actualCC == "" && actualLat != 0 && actualLng != 0 {
			actualCC = ci.code(actualLat, actualLng)
		}

		// Calculate accurate distance using Haversine formula
		calculatedDistance := haversineDistance(guess.Lat, guess.Lng, actualLat, actualLng)

		debugLog("storeStandard: Round %d: score=%.0f, guess=(%.4f,%.4f), actual=(%.4f,%.4f), distance=%.2fkm, guessed_cc=%s, actual_cc=%s",
			i+1, guess.RoundScoreInPoints, guess.Lat, guess.Lng, actualLat, actualLng, calculatedDistance, guessedCC, actualCC)

		_, err := stmt.Exec(
			id, i+1, guess.RoundScoreInPoints,
			guess.Lat, guess.Lng, calculatedDistance, guessedCC,
			actualLat, actualLng, actualCC,
			guess.Time, guess.StepsCount, guess.TimedOut || guess.TimedOutWithGuess, guess.RoundScoreInPercentage,
		)
		if err != nil {
			debugLog("storeStandard: Error inserting round %d for game %s: %v", i+1, id, err)
		}
	}
	stmt.Close()
	err = tx.Commit()
	if err != nil {
		debugLog("storeStandard: Error committing transaction for game %s: %v", id, err)
	} else {
		debugLog("storeStandard: Successfully stored game %s with %d rounds", id, len(g.Player.Guesses))
	}
}

func storeDuels(id string, ci *countryIndex) {
	if rowExists(`SELECT 1 FROM rounds WHERE game_id=? LIMIT 1`, id) {
		return
	}
	resp, err := apiClient().Get("https://www.geoguessr.com/duels/" + id + "/summary")
	if err != nil {
		log.Println("duel fetch", err)
		return
	}
	html, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := nextDataRE.FindSubmatch(html)
	if len(m) < 2 {
		log.Println("no __NEXT_DATA__", id)
		return
	}
	var d v4Summary
	if err = json.Unmarshal(m[1], &d); err != nil {
		log.Println("duel JSON", err)
		return
	}

	opts := d.Props.PageProps.Game.Options.MovementOptions
	mov := mode(opts.ForbidMoving, opts.ForbidZooming, opts.ForbidRotating)

	// Extract game date from first round's start time if available
	var gameDate string
	if len(d.Props.PageProps.Game.Rounds) > 0 && d.Props.PageProps.Game.Rounds[0].StartTime > 0 {
		// Convert Unix timestamp (milliseconds) to string for normalization
		gameDate = fmt.Sprintf("%d", d.Props.PageProps.Game.Rounds[0].StartTime)
	}

	// Extract duels result information
	result := d.Props.PageProps.Game.Result
	isDraw := fmt.Sprintf("%t", result.IsDraw)
	winningTeamId := result.WinningTeamId
	winnerStyle := result.WinnerStyle

	// Extract opponent information and player team ID
	uid := d.Props.PageProps.UserId
	var opponentId, opponentNick, playerTeamId string

	for _, t := range d.Props.PageProps.Game.Teams {
		if len(t.Players) == 0 {
			continue
		}

		if t.Players[0].PlayerId == uid {
			// This is the player's team
			playerTeamId = t.Id
		} else {
			// This is the opponent's team
			opponentId = t.Players[0].PlayerId
			opponentNick = t.Players[0].Nick
		}
	}

	insertGame(id, "duels", mov, gameDate, "", isDraw, winningTeamId, winnerStyle, opponentId, opponentNick, playerTeamId)

	type GuessData struct {
		RoundNumber int
		Score       float64
		Lat, Lng    float64
		Distance    float64
	}
	type HealthData struct {
		RoundNumber  int
		HealthBefore int
		HealthAfter  int
	}

	var you, opp []GuessData
	var youHealth, oppHealth []HealthData

	for _, t := range d.Props.PageProps.Game.Teams {
		if len(t.Players) == 0 {
			continue
		}
		// Convert the guesses to our local type
		var guesses []GuessData
		for _, g := range t.Players[0].Guesses {
			guesses = append(guesses, GuessData{
				RoundNumber: g.RoundNumber,
				Score:       g.Score,
				Lat:         g.Lat,
				Lng:         g.Lng,
				Distance:    g.Distance,
			})
		}

		// Extract health data
		var health []HealthData
		for _, r := range t.RoundResults {
			health = append(health, HealthData{
				RoundNumber:  r.RoundNumber,
				HealthBefore: r.HealthBefore,
				HealthAfter:  r.HealthAfter,
			})
		}

		if t.Players[0].PlayerId == uid {
			you = guesses
			youHealth = health
		} else {
			opp = guesses
			oppHealth = health
		}
	}

	// Create lookup maps
	oppMap := map[int]struct{ Score, Lat, Lng, Dist float64 }{}
	for _, g := range opp {
		oppMap[g.RoundNumber] = struct{ Score, Lat, Lng, Dist float64 }{g.Score, g.Lat, g.Lng, g.Distance}
	}

	youHealthMap := map[int]struct{ Before, After int }{}
	for _, h := range youHealth {
		youHealthMap[h.RoundNumber] = struct{ Before, After int }{h.HealthBefore, h.HealthAfter}
	}

	oppHealthMap := map[int]struct{ Before, After int }{}
	for _, h := range oppHealth {
		oppHealthMap[h.RoundNumber] = struct{ Before, After int }{h.HealthBefore, h.HealthAfter}
	}

	// Create rounds lookup for actual locations
	roundsMap := map[int]struct {
		ActualLat, ActualLng float64
		ActualCountry        string
		Multiplier           float64
		StartTime, EndTime   int64
	}{}
	for _, r := range d.Props.PageProps.Game.Rounds {
		roundsMap[r.RoundNumber] = struct {
			ActualLat, ActualLng float64
			ActualCountry        string
			Multiplier           float64
			StartTime, EndTime   int64
		}{
			ActualLat:     r.Panorama.Lat,
			ActualLng:     r.Panorama.Lng,
			ActualCountry: r.Panorama.CountryCode,
			Multiplier:    r.Multiplier,
			StartTime:     r.StartTime,
			EndTime:       r.EndTime,
		}
	}

	tx, _ := db.Begin()
	stmt, _ := tx.Prepare(`INSERT OR IGNORE INTO rounds(
		game_id, round_no, player_score, opponent_score,
		player_lat, player_lng, opponent_lat, opponent_lng,
		player_dist, opponent_dist, country_code,
		actual_lat, actual_lng, actual_country_code,
		round_multiplier,
		player_health_before, player_health_after,
		opponent_health_before, opponent_health_after,
		round_start_time, round_end_time
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)

	for _, g := range you {
		o := oppMap[g.RoundNumber]
		yh := youHealthMap[g.RoundNumber]
		oh := oppHealthMap[g.RoundNumber]
		r := roundsMap[g.RoundNumber]

		cc := ci.code(g.Lat, g.Lng)

		// Calculate accurate distances using Haversine formula
		playerDistance := haversineDistance(g.Lat, g.Lng, r.ActualLat, r.ActualLng)
		opponentDistance := haversineDistance(o.Lat, o.Lng, r.ActualLat, r.ActualLng)

		_, _ = stmt.Exec(
			id, g.RoundNumber, g.Score, o.Score,
			g.Lat, g.Lng, o.Lat, o.Lng,
			playerDistance, opponentDistance, cc,
			r.ActualLat, r.ActualLng, r.ActualCountry,
			r.Multiplier,
			yh.Before, yh.After,
			oh.Before, oh.After,
			r.StartTime, r.EndTime,
		)
	}
	stmt.Close()
	tx.Commit()
}

func rowExists(q string, args ...interface{}) bool {
	var tmp int
	err := db.QueryRow(q, args...).Scan(&tmp)
	return err == nil
}

// ------------------------------------------------------------
// API helpers

type agg struct {
	TotalGames       int
	TotalRounds      int
	AvgScore         float64
	AvgDistKm        float64
	FavouriteCountry string
	BestCountry      string
	WorstCountry     string
}

func summaryStats(gameType, movement string) (agg, error) {
	if gameType == "" {
		gameType = "standard"
	}
	if movement == "" {
		movement = ""
	} // empty means any
	var a agg
	whereGames := "WHERE game_type=?"
	args := []interface{}{gameType}
	if movement != "" {
		whereGames += " AND movement=?"
		args = append(args, movement)
	}
	// total games / rounds
	db.QueryRow("SELECT COUNT(*) FROM games "+whereGames, args...).Scan(&a.TotalGames)
	db.QueryRow("SELECT COUNT(*) FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames, args...).Scan(&a.TotalRounds)
	// avg score & dist
	db.QueryRow("SELECT COALESCE(AVG(player_score),0) FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames, args...).Scan(&a.AvgScore)
	db.QueryRow("SELECT COALESCE(AVG(player_dist),0) FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames, args...).Scan(&a.AvgDistKm)
	// favourite (most) - use actual country when available, fallback to guessed country
	rows, _ := db.Query("SELECT COALESCE(actual_country_code, country_code) as display_country, COUNT(*) c FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames+" GROUP BY display_country ORDER BY c DESC LIMIT 1", args...)
	for rows.Next() {
		var countryCode string
		rows.Scan(&countryCode, new(int))
		a.FavouriteCountry = countryCoder.NameEnByCode(countryCode)
	}
	rows.Close()
	// best/worst by avg score - use actual country when available
	var bestCountry, worstCountry string
	bestRow := db.QueryRow("SELECT COALESCE(actual_country_code, country_code) as display_country FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames+" GROUP BY display_country HAVING display_country != '??' AND display_country != '' AND COUNT(*) >= 1 ORDER BY AVG(player_score) DESC LIMIT 1", args...)
	if err := bestRow.Scan(&bestCountry); err == nil {
		a.BestCountry = countryCoder.NameEnByCode(bestCountry)
	} else if err != sql.ErrNoRows {
		debugLog("Best country query error: %v", err)
	}
	// If err == sql.ErrNoRows, BestCountry remains "-" (empty string)

	worstRow := db.QueryRow("SELECT COALESCE(actual_country_code, country_code) as display_country FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames+" GROUP BY display_country HAVING display_country != '??' AND display_country != '' AND COUNT(*) >= 1 ORDER BY AVG(player_score) ASC LIMIT 1", args...)
	if err := worstRow.Scan(&worstCountry); err == nil {
		a.WorstCountry = countryCoder.NameEnByCode(worstCountry)
	} else if err != sql.ErrNoRows {
		debugLog("Worst country query error: %v", err)
	}
	// If err == sql.ErrNoRows, WorstCountry remains "-" (empty string)
	return a, nil
}

// Enhanced summary stats with timeline filtering
func summaryStatsWithTimeline(gameType, movement string, timelineDays int) (*agg, error) {
	if gameType == "" {
		gameType = "standard"
	}

	var a agg
	whereGames := "WHERE game_type=?"
	args := []interface{}{gameType}

	if movement != "" {
		whereGames += " AND movement=?"
		args = append(args, movement)
	}

	// Add timeline filter if specified
	if timelineDays > 0 {
		whereGames += " AND game_date >= datetime('now', '-' || ? || ' days')"
		args = append(args, timelineDays)
	}

	// Use the existing summaryStats logic but with timeline filter
	db.QueryRow("SELECT COUNT(*) FROM games "+whereGames, args...).Scan(&a.TotalGames)
	db.QueryRow("SELECT COUNT(*) FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames, args...).Scan(&a.TotalRounds)
	db.QueryRow("SELECT COALESCE(AVG(player_score),0) FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames, args...).Scan(&a.AvgScore)
	db.QueryRow("SELECT COALESCE(AVG(player_dist),0) FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames, args...).Scan(&a.AvgDistKm)

	// favourite (most) - use actual country when available, fallback to guessed country
	rows, _ := db.Query("SELECT COALESCE(actual_country_code, country_code) as display_country, COUNT(*) c FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames+" GROUP BY display_country ORDER BY c DESC LIMIT 1", args...)
	for rows.Next() {
		var countryCode string
		rows.Scan(&countryCode, new(int))
		a.FavouriteCountry = countryCoder.NameEnByCode(countryCode)
	}
	rows.Close()

	// best/worst by avg score - use actual country when available
	var bestCountry, worstCountry string
	bestRow := db.QueryRow("SELECT COALESCE(actual_country_code, country_code) as display_country FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames+" GROUP BY display_country HAVING display_country != '??' AND display_country != '' AND COUNT(*) >= 1 ORDER BY AVG(player_score) DESC LIMIT 1", args...)
	if err := bestRow.Scan(&bestCountry); err == nil {
		a.BestCountry = countryCoder.NameEnByCode(bestCountry)
	} else if err != sql.ErrNoRows {
		debugLog("Best country query error: %v", err)
	}
	// If err == sql.ErrNoRows, BestCountry remains "-" (empty string)

	worstRow := db.QueryRow("SELECT COALESCE(actual_country_code, country_code) as display_country FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames+" GROUP BY display_country HAVING display_country != '??' AND display_country != '' AND COUNT(*) >= 1 ORDER BY AVG(player_score) ASC LIMIT 1", args...)
	if err := worstRow.Scan(&worstCountry); err == nil {
		a.WorstCountry = countryCoder.NameEnByCode(worstCountry)
	} else if err != sql.ErrNoRows {
		debugLog("Worst country query error: %v", err)
	}
	// If err == sql.ErrNoRows, WorstCountry remains "-" (empty string)

	return &a, nil
}

func apiSummary(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type") // standard|duels
	mov := r.URL.Query().Get("move") // Moving|NoMove|NMPZ
	timeline := r.URL.Query().Get("timeline")

	var res *agg
	//var err error

	if timeline != "" {
		if days, errConv := strconv.Atoi(timeline); errConv == nil && days > 0 {
			res, _ = summaryStatsWithTimeline(typ, mov, days)
		} else {
			tmp, _ := summaryStats(typ, mov)
			res = &tmp
		}
	} else {
		tmp, _ := summaryStats(typ, mov)
		res = &tmp
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func apiGames(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	limit := 30

	var rows *sql.Rows
	var err error

	if typ == "duels" {
		// For duels, use the stored game result to determine win/loss
		rows, err = db.Query(`
			SELECT g.id, g.movement, g.created, g.game_date,
				   CASE
					   WHEN g.is_draw = 1 THEN 'draw'
					   WHEN g.winning_team_id IS NOT NULL AND g.player_team_id IS NOT NULL THEN
						   CASE WHEN g.winning_team_id = g.player_team_id THEN 'win' ELSE 'loss' END
					   ELSE 'unknown'
				   END as result
			FROM games g
			WHERE g.game_type=?
			ORDER BY COALESCE(g.game_date, g.created) DESC
			LIMIT ?`, typ, limit)
	} else {
		// For standard games, include map name and total score
		rows, err = db.Query(`
			SELECT g.id, g.movement, g.created, g.game_date,
				   COALESCE(g.map_name, '') as map_name,
				   COALESCE(SUM(r.player_score), 0) as total_score
			FROM games g
			LEFT JOIN rounds r ON g.id = r.game_id
			WHERE g.game_type=?
			GROUP BY g.id, g.movement, g.created, g.game_date, g.map_name
			ORDER BY COALESCE(g.game_date, g.created) DESC
			LIMIT ?`, typ, limit)
	}

	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var id, mov, ts string
		var gameDate *string
		var result *string
		var mapName *string
		var totalScore *float64

		if typ == "duels" {
			rows.Scan(&id, &mov, &ts, &gameDate, &result)
		} else {
			rows.Scan(&id, &mov, &ts, &gameDate, &mapName, &totalScore)
		}

		game := map[string]any{
			"id":       id,
			"movement": mov,
			"created":  ts,
		}

		if gameDate != nil {
			game["gameDate"] = *gameDate
		}

		if result != nil {
			game["result"] = *result
		}

		if mapName != nil && *mapName != "" {
			game["mapName"] = *mapName
		}

		if totalScore != nil {
			game["totalScore"] = int(*totalScore)
		}

		out = append(out, game)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func apiGame(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "game id required", 400)
		return
	}

	// First get game info including map name, opponent_id, and opponent_nick
	var gameType, mapName, opponentId, opponentNick string
	gameRow := db.QueryRow(`SELECT game_type, COALESCE(map_name, ''), COALESCE(opponent_id, ''), COALESCE(opponent_nick, '') FROM games WHERE id=?`, id)
	err := gameRow.Scan(&gameType, &mapName, &opponentId, &opponentNick)
	if err != nil {
		debugLog("Error fetching game info for id %s: %v", id, err)
		http.Error(w, "game not found", 404)
		return
	}

	// Get round data with enhanced fields for singleplayer
	var query string
	if gameType == "standard" {
		query = `SELECT round_no,player_score,opponent_score,player_lat,player_lng,country_code,actual_country_code,
				round_time,steps_count,timed_out,score_percentage,player_dist
				FROM rounds WHERE game_id=? ORDER BY round_no`
	} else {
		query = `SELECT round_no,player_score,opponent_score,player_lat,player_lng,country_code,actual_country_code,
				0 as round_time,0 as steps_count,0 as timed_out,0 as score_percentage,player_dist
				FROM rounds WHERE game_id=? ORDER BY round_no`
	}

	rows, err := db.Query(query, id)
	if err != nil {
		debugLog("Error querying rounds for game %s: %v", id, err)
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	result := map[string]any{
		"gameType": gameType,
		"mapName":  mapName,
		"rounds":   []map[string]any{},
	}
	if gameType == "duels" {
		result["opponentId"] = opponentId
		result["opponentNick"] = opponentNick
	}

	for rows.Next() {
		var rn, roundTime, stepsCount int
		var ps, lat, lng, scorePercentage, playerDist float64
		var os sql.NullFloat64 // Handle NULL opponent scores for single-player games
		var cc, actualCC string
		var timedOut bool

		err := rows.Scan(&rn, &ps, &os, &lat, &lng, &cc, &actualCC, &roundTime, &stepsCount, &timedOut, &scorePercentage, &playerDist)
		if err != nil {
			debugLog("Error scanning round data for game %s: %v", id, err)
			continue
		}

		// Use actual country for display, fallback to guessed country if actual is not available
		displayCountryCode := actualCC
		if displayCountryCode == "" {
			displayCountryCode = cc
		}

		// Handle opponent score - use 0 if NULL (single-player game)
		var opponentScore float64
		if os.Valid {
			opponentScore = os.Float64
		} else {
			opponentScore = 0
		}

		roundData := map[string]any{
			"round":          rn,
			"player":         ps,
			"opponent":       opponentScore,
			"lat":            lat,
			"lng":            lng,
			"cc":             displayCountryCode,
			"country":        countryCoder.NameEnByCode(displayCountryCode),
			"guessedCountry": countryCoder.NameEnByCode(cc), // Keep guessed country for reference
		}

		// Add enhanced data for singleplayer games
		if gameType == "standard" {
			roundData["time"] = roundTime
			roundData["stepsCount"] = stepsCount
			roundData["timedOut"] = timedOut
			roundData["scorePercentage"] = scorePercentage
			roundData["distance"] = playerDist // Already in km
		}

		result["rounds"] = append(result["rounds"].([]map[string]any), roundData)
	}
	rows.Close()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// API endpoint for individual game map data with geographic coordinates
func apiGameMapData(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "game id required", 400)
		return
	}

	// Query both player and actual location data for the game map
	rows, err := db.Query(`
		SELECT round_no, player_score, opponent_score, player_lat, player_lng,
		       opponent_lat, opponent_lng, country_code, actual_lat, actual_lng,
		       actual_country_code, player_dist
		FROM rounds
		WHERE game_id=?
		ORDER BY round_no`, id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var rn int
		var ps, playerLat, playerLng, playerDist float64
		var os sql.NullFloat64 // Handle NULL opponent scores
		var opponentLatPtr, opponentLngPtr *float64
		var cc string
		var actualLatPtr, actualLngPtr *float64
		var actualCountryPtr *string

		err := rows.Scan(&rn, &ps, &os, &playerLat, &playerLng, &opponentLatPtr, &opponentLngPtr, &cc, &actualLatPtr, &actualLngPtr, &actualCountryPtr, &playerDist)
		if err != nil {
			debugLog("Error scanning game map data for game %s: %v", id, err)
			continue
		}

		// Handle opponent score - use 0 if NULL (single-player game)
		var opponentScore float64
		if os.Valid {
			opponentScore = os.Float64
		} else {
			opponentScore = 0
		}

		roundData := map[string]any{
			"round":       rn,
			"playerScore": ps,
			"oppScore":    opponentScore,
			"playerLat":   playerLat,
			"playerLng":   playerLng,
			"distance":    playerDist, // Already in km
		}

		// Use actual country for the target location, fallback to guessed country if not available
		if actualCountryPtr != nil && *actualCountryPtr != "" {
			roundData["country"] = countryCoder.NameEnByCode(*actualCountryPtr)
			roundData["countryCode"] = *actualCountryPtr
			roundData["actualCountry"] = countryCoder.NameEnByCode(*actualCountryPtr)
		} else {
			roundData["country"] = countryCoder.NameEnByCode(cc)
			roundData["countryCode"] = cc
		}

		// Also include the guessed country for reference
		roundData["guessedCountry"] = countryCoder.NameEnByCode(cc)
		roundData["guessedCountryCode"] = cc

		// Add opponent coordinates if available
		if opponentLatPtr != nil && opponentLngPtr != nil {
			roundData["opponentLat"] = *opponentLatPtr
			roundData["opponentLng"] = *opponentLngPtr
		}

		// Add actual location if available
		if actualLatPtr != nil && actualLngPtr != nil {
			roundData["actualLat"] = *actualLatPtr
			roundData["actualLng"] = *actualLngPtr
		}

		out = append(out, roundData)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// update cookie
func apiUpdateCookie(w http.ResponseWriter, r *http.Request) {
	t := r.URL.Query().Get("token")
	if t == "" {
		http.Error(w, "token= missing", 400)
		return
	}

	// Check private key if in public mode
	if config.IsPublic {
		key := r.URL.Query().Get("key")
		if key != config.PrivateKey {
			http.Error(w, "unauthorized", 401)
			return
		}
	}

	config.NCFA = t
	if err := saveConfig(config); err != nil {
		debugLog("Failed to save config after NCFA update: %v", err)
	}
	fmt.Fprintln(w, "cookie updated")
}

// trigger collection
func apiCollectNow(w http.ResponseWriter, r *http.Request) {
	// Check if NCFA is set
	if config.NCFA == "" {
		http.Error(w, "NCFA cookie not set. Please update your cookie first using /api/update_ncfa", 400)
		return
	}

	// Check private key if in public mode
	if config.IsPublic {
		key := r.URL.Query().Get("key")
		if key != config.PrivateKey {
			http.Error(w, "unauthorized", 401)
			return
		}
	}

	debugLog("Collection triggered via API")
	ci := loadCountries()

	// First, collect user profile data
	debugLog("Collecting user profile data...")
	if err := collectUserProfile(); err != nil {
		debugLog("Warning: Failed to collect user profile data: %v", err)
		// Continue with game collection even if profile collection fails
	}

	debugLog("Starting pullFeed...")
	std, duels := pullFeed()
	debugLog("pullFeed returned: %d standard games, %d duels games", len(std), len(duels))

	// Log the actual game IDs we got
	if len(std) > 0 {
		debugLog("Standard game IDs: %v", std)
	}
	if len(duels) > 0 {
		debugLog("Duels game IDs: %v", duels)
	}

	// Track successful imports by checking if games existed before
	stdSuccess := 0
	duelsSuccess := 0

	debugLog("Starting to store standard games...")
	for i, g := range std {
		debugLog("Storing standard game %d/%d: %s", i+1, len(std), g)
		existed := rowExists(`SELECT 1 FROM rounds WHERE game_id=? LIMIT 1`, g)
		storeStandard(g, ci)
		if !existed && rowExists(`SELECT 1 FROM rounds WHERE game_id=? LIMIT 1`, g) {
			stdSuccess++
		}
	}

	debugLog("Starting to store duels games...")
	for i, d := range duels {
		debugLog("Storing duels game %d/%d: %s", i+1, len(duels), d)
		existed := rowExists(`SELECT 1 FROM rounds WHERE game_id=? LIMIT 1`, d)
		storeDuels(d, ci)
		if !existed && rowExists(`SELECT 1 FROM rounds WHERE game_id=? LIMIT 1`, d) {
			duelsSuccess++
		}
	}

	debugLog("Collection complete")

	// Prepare enhanced response
	response := map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Collection completed! Found %d new games (%d singleplayer, %d duels)",
			stdSuccess+duelsSuccess, stdSuccess, duelsSuccess),
		"details": map[string]int{
			"Singleplayer": stdSuccess,
			"Duels":        duelsSuccess,
		},
		"total": stdSuccess + duelsSuccess,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// ------------------------------------------------------------
// Enhanced API endpoints for dashboard

type CountryStats struct {
	Country     string  `json:"country"`
	CountryCode string  `json:"countryCode"`
	PointsLost  float64 `json:"pointsLost"`
	Distance    float64 `json:"distance"`
	Count       int     `json:"count"`
	AvgScore    float64 `json:"avgScore"`
}

type ChartData struct {
	Labels   []string  `json:"labels"`
	Datasets []Dataset `json:"datasets"`
}

type Dataset struct {
	Label           string    `json:"label"`
	Data            []float64 `json:"data"`
	BackgroundColor string    `json:"backgroundColor"`
	BorderColor     string    `json:"borderColor"`
	TotalRounds     []float64 `json:"totalRounds,omitempty"`
}

type CountryConfusion struct {
	GuessedCountry string  `json:"guessedCountry"`
	ActualCountry  string  `json:"actualCountry"`
	Count          int     `json:"count"`
	AvgDistance    float64 `json:"avgDistance"`
}

func apiCountryStats(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	mov := r.URL.Query().Get("move")
	timeline := r.URL.Query().Get("timeline")

	whereGames := "WHERE game_type=?"
	args := []interface{}{typ}
	if mov != "" {
		whereGames += " AND movement=?"
		args = append(args, mov)
	}
	if timeline != "" {
		if days, err := strconv.Atoi(timeline); err == nil && days > 0 {
			whereGames += " AND game_date >= datetime('now', '-' || ? || ' days')"
			args = append(args, days)
		}
	}

	query := `SELECT COALESCE(actual_country_code, country_code) as display_country,
		AVG(5000 - player_score) as points_lost,
		AVG(player_dist) as avg_distance,
		COUNT(*) as count,
		AVG(player_score) as avg_score
		FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
		GROUP BY display_country HAVING display_country != '??' ORDER BY points_lost DESC`

	rows, err := db.Query(query, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var stats []CountryStats
	for rows.Next() {
		var s CountryStats
		var countryCode string
		err := rows.Scan(&countryCode, &s.PointsLost, &s.Distance, &s.Count, &s.AvgScore)
		if err != nil {
			debugLog("Error scanning country stats row: %v", err)
			continue
		}
		s.Country = countryCoder.NameEnByCode(countryCode) // Convert code to proper name
		s.CountryCode = strings.ToUpper(countryCode)       // Include the country code
		stats = append(stats, s)
	}

	// Ensure we always return an array, even if empty
	if stats == nil {
		stats = []CountryStats{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func apiChartData(w http.ResponseWriter, r *http.Request) {
	chartType := r.URL.Query().Get("chart")
	gameType := r.URL.Query().Get("type")
	mov := r.URL.Query().Get("move")
	timeline := r.URL.Query().Get("timeline")

	whereGames := "WHERE game_type=?"
	args := []interface{}{gameType}
	if mov != "" {
		whereGames += " AND movement=?"
		args = append(args, mov)
	}
	if timeline != "" {
		if days, err := strconv.Atoi(timeline); err == nil && days > 0 {
			whereGames += " AND game_date >= datetime('now', '-' || ? || ' days')"
			args = append(args, days)
		}
	}

	var chartData ChartData

	switch chartType {
	case "countries", "countryPerformance":
		// Most frequent countries - use actual country when available
		query := `SELECT COALESCE(actual_country_code, country_code) as display_country, COUNT(*) as count
			FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
			GROUP BY display_country HAVING display_country != '??'
			ORDER BY count DESC LIMIT 10`

		rows, _ := db.Query(query, args...)
		var labels []string
		var data []float64

		for rows.Next() {
			var countryCode string
			var count float64
			rows.Scan(&countryCode, &count)
			labels = append(labels, countryCoder.NameEnByCode(countryCode))
			data = append(data, count)
		}
		rows.Close()

		chartData = ChartData{
			Labels: labels,
			Datasets: []Dataset{{
				Label:           "Games Played",
				Data:            data,
				BackgroundColor: "rgba(54, 162, 235, 0.6)",
				BorderColor:     "rgba(54, 162, 235, 1)",
			}},
		}

	case "scoreDistribution":
		// Score distribution histogram
		query := `SELECT player_score FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames
		rows, _ := db.Query(query, args...)

		// Create buckets for score ranges
		buckets := map[string]int{
			"0-1000":    0,
			"1000-2000": 0,
			"2000-3000": 0,
			"3000-4000": 0,
			"4000-5000": 0,
		}

		for rows.Next() {
			var score float64
			rows.Scan(&score)
			switch {
			case score < 1000:
				buckets["0-1000"]++
			case score < 2000:
				buckets["1000-2000"]++
			case score < 3000:
				buckets["2000-3000"]++
			case score < 4000:
				buckets["3000-4000"]++
			default:
				buckets["4000-5000"]++
			}
		}
		rows.Close()

		labels := []string{"0-1000", "1000-2000", "2000-3000", "3000-4000", "4000-5000"}
		var data []float64
		for _, label := range labels {
			data = append(data, float64(buckets[label]))
		}

		chartData = ChartData{
			Labels: labels,
			Datasets: []Dataset{{
				Label:           "Number of Rounds",
				Data:            data,
				BackgroundColor: "rgba(255, 99, 132, 0.6)",
				BorderColor:     "rgba(255, 99, 132, 1)",
			}},
		}

	// case "countryPerformance":
	// 	// Country performance with win rates for duels
	// 	if gameType == "duels" {
	// 		query := `SELECT COALESCE(actual_country_code, country_code) as display_country,
	// 			COUNT(DISTINCT g.id) as total,
	// 			SUM(CASE
	// 				WHEN g.is_draw = 1 THEN 0
	// 				WHEN g.winning_team_id IS NOT NULL THEN
	// 					(SELECT CASE
	// 						WHEN COUNT(CASE WHEN r3.player_score > r3.opponent_score THEN 1 END) > COUNT(CASE WHEN r3.player_score < r3.opponent_score THEN 1 END)
	// 						THEN 1 ELSE 0 END
	// 					FROM rounds r3
	// 					WHERE r3.game_id = g.id)
	// 				ELSE 0
	// 			END) as wins
	// 			FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
	// 			GROUP BY display_country HAVING display_country != '??' AND total >= 1
	// 			ORDER BY total DESC LIMIT 10`

	// 		rows, _ := db.Query(query, args...)
	// 		var labels []string
	// 		var totalData []float64
	// 		var winData []float64

	// 		for rows.Next() {
	// 			var countryCode string
	// 			var total, wins int
	// 			rows.Scan(&countryCode, &total, &wins)
	// 			labels = append(labels, countryCoder.NameEnByCode(countryCode))
	// 			totalData = append(totalData, float64(total))
	// 			winData = append(winData, float64(wins))
	// 		}
	// 		rows.Close()

	// 		chartData = ChartData{
	// 			Labels: labels,
	// 			Datasets: []Dataset{
	// 				{
	// 					Label:           "Total Rounds",
	// 					Data:            totalData,
	// 					BackgroundColor: "rgba(54, 162, 235, 0.6)",
	// 					BorderColor:     "rgba(54, 162, 235, 1)",
	// 				},
	// 				{
	// 					Label:           "Wins",
	// 					Data:            winData,
	// 					BackgroundColor: "rgba(75, 192, 192, 0.6)",
	// 					BorderColor:     "rgba(75, 192, 192, 1)",
	// 				},
	// 			},
	// 		}
	// 	} else {
	// 		// For standard games, show average score by country only
	// 		query := `SELECT COALESCE(actual_country_code, country_code) as display_country,
	// 			COUNT(*) as total,
	// 			AVG(player_score) as avg_score
	// 			FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
	// 			GROUP BY display_country HAVING display_country != '??' AND total >= 1
	// 			ORDER BY total DESC LIMIT 10`

	// 		rows, _ := db.Query(query, args...)
	// 		var labels []string
	// 		var scoreData []float64
	// 		var totalData []float64 // Store for tooltip data

	// 		for rows.Next() {
	// 			var countryCode string
	// 			var total int
	// 			var avgScore float64
	// 			rows.Scan(&countryCode, &total, &avgScore)
	// 			labels = append(labels, countryCoder.NameEnByCode(countryCode))
	// 			scoreData = append(scoreData, avgScore)
	// 			totalData = append(totalData, float64(total)) // Store for tooltip
	// 		}
	// 		rows.Close()

	// 		chartData = ChartData{
	// 			Labels: labels,
	// 			Datasets: []Dataset{
	// 				{
	// 					Label:           "Avg Score",
	// 					Data:            scoreData,
	// 					BackgroundColor: "rgba(255, 206, 86, 0.6)",
	// 					BorderColor:     "rgba(255, 206, 86, 1)",
	// 					TotalRounds:     totalData, // Add total rounds for tooltip
	// 				},
	// 			},
	// 		}
	// 	}

	case "winRate":
		// Win rate by country for duels
		if gameType == "duels" {
			query := `SELECT COALESCE(actual_country_code, country_code) as display_country,
				COUNT(DISTINCT g.id) as total,
				SUM(CASE
					WHEN g.is_draw = 1 THEN 0
					WHEN g.winning_team_id IS NOT NULL AND g.player_team_id IS NOT NULL THEN
						CASE WHEN g.winning_team_id = g.player_team_id THEN 1 ELSE 0 END
					ELSE 0
				END) as wins
				FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
				GROUP BY display_country HAVING display_country != '??' AND total >= 2
				ORDER BY total DESC LIMIT 10`

			rows, _ := db.Query(query, args...)
			var labels []string
			var totalData []float64
			var winData []float64

			for rows.Next() {
				var country string
				var total, wins int
				rows.Scan(&country, &total, &wins)
				labels = append(labels, country)
				totalData = append(totalData, float64(total))
				winData = append(winData, float64(wins))
			}
			rows.Close()

			chartData = ChartData{
				Labels: labels,
				Datasets: []Dataset{
					{
						Label:           "Total Games",
						Data:            totalData,
						BackgroundColor: "rgba(54, 162, 235, 0.6)",
						BorderColor:     "rgba(54, 162, 235, 1)",
					},
					{
						Label:           "Wins",
						Data:            winData,
						BackgroundColor: "rgba(75, 192, 192, 0.6)",
						BorderColor:     "rgba(75, 192, 192, 1)",
					},
				},
			}
		}

	case "confusedCountries":
		// Most confused country pairs - where players guess one country but it's actually another
		query := `SELECT country_code, actual_country_code, COUNT(*) as confusion_count
			FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
			AND country_code != '??' AND actual_country_code != '??'
			AND country_code != actual_country_code
			GROUP BY country_code, actual_country_code
			HAVING confusion_count >= 2
			ORDER BY confusion_count DESC LIMIT 10`

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		var labels []string
		var data []float64

		for rows.Next() {
			var guessedCode, actualCode string
			var count float64
			rows.Scan(&guessedCode, &actualCode, &count)

			// Create label showing "Guessed Country → Actual Country"
			guessedName := countryCoder.NameEnByCode(guessedCode)
			actualName := countryCoder.NameEnByCode(actualCode)
			label := guessedName + " → " + actualName

			labels = append(labels, label)
			data = append(data, count)
		}

		chartData = ChartData{
			Labels: labels,
			Datasets: []Dataset{{
				Label:           "Confusion Count",
				Data:            data,
				BackgroundColor: "rgba(255, 99, 132, 0.6)",
				BorderColor:     "rgba(255, 99, 132, 1)",
			}},
		}

	case "weeklyPerformance":
		// Weekly performance showing average score and distance
		query := `SELECT strftime('%Y-%W', COALESCE(g.game_date, g.created)) as week,
			AVG(r.player_score) as avg_score,
			AVG(r.player_dist) as avg_distance,
			COUNT(*) as round_count
			FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
			GROUP BY week
			HAVING round_count >= 1
			ORDER BY week`

		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		var labels []string
		var scoreData []float64
		var distanceData []float64

		for rows.Next() {
			var week string
			var avgScore, avgDistance float64
			var roundCount int
			rows.Scan(&week, &avgScore, &avgDistance, &roundCount)

			// Format week label (e.g., "2025-24" -> "Week 24")
			weekNum := week[5:] // Extract week number from "YYYY-WW"
			labels = append(labels, "Week "+weekNum)
			scoreData = append(scoreData, avgScore)
			distanceData = append(distanceData, avgDistance)
		}

		chartData = ChartData{
			Labels: labels,
			Datasets: []Dataset{
				{
					Label:           "Average Score",
					Data:            scoreData,
					BackgroundColor: "rgba(52, 152, 219, 0.1)",
					BorderColor:     "rgba(52, 152, 219, 1)",
				},
				{
					Label:           "Average Distance (km)",
					Data:            distanceData,
					BackgroundColor: "rgba(231, 76, 60, 0.1)",
					BorderColor:     "rgba(231, 76, 60, 1)",
				},
			},
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chartData)
}

// ------------------------------------------------------------
// Country page API endpoints

type CountrySummary struct {
	TotalGames       int     `json:"totalGames"`
	TotalRounds      int     `json:"totalRounds"`
	AvgScore         float64 `json:"avgScore"`
	AvgDistance      float64 `json:"avgDistance"`
	MostConfusedWith string  `json:"mostConfusedWith"`
}

type CountryRound struct {
	GameId        string   `json:"gameId"`
	RoundNumber   int      `json:"roundNumber"`
	PlayerScore   float64  `json:"playerScore"`
	OpponentScore *float64 `json:"opponentScore,omitempty"`
	Distance      float64  `json:"distance"`
	ActualLat     float64  `json:"actualLat"`
	ActualLng     float64  `json:"actualLng"`
	PlayerLat     float64  `json:"playerLat"`
	PlayerLng     float64  `json:"playerLng"`
	Created       string   `json:"created"`
	GameDate      *string  `json:"gameDate,omitempty"`
	Movement      string   `json:"movement"`
	Time          *int     `json:"time,omitempty"`
	Steps         *int     `json:"steps,omitempty"`
}

func apiCountrySummary(w http.ResponseWriter, r *http.Request) {
	// Extract country code from URL path
	path := r.URL.Path
	parts := strings.Split(path, "/")
	if len(parts) < 5 || parts[2] != "country" || parts[4] != "summary" {
		http.Error(w, "Invalid country summary path", 400)
		return
	}
	countryCode := strings.ToUpper(parts[3])

	typ := r.URL.Query().Get("type")
	mov := r.URL.Query().Get("move")
	timeline := r.URL.Query().Get("timeline")

	if typ == "" {
		typ = "standard"
	}

	// Build query conditions - use LIKE to handle compound country codes like "id|id", "ph|id", etc.
	whereGames := "WHERE game_type=? AND (COALESCE(actual_country_code, country_code) LIKE '%' || ? || '%')"
	args := []interface{}{typ, strings.ToLower(countryCode)}

	if mov != "" {
		whereGames += " AND movement=?"
		args = append(args, mov)
	}

	// Add timeline filter if specified
	if timeline != "" {
		if days, err := strconv.Atoi(timeline); err == nil && days > 0 {
			whereGames += " AND game_date >= datetime('now', '-' || ? || ' days')"
			args = append(args, days)
		}
	}

	var summary CountrySummary

	// Get total games and rounds
	db.QueryRow("SELECT COUNT(DISTINCT g.id) FROM games g JOIN rounds r ON g.id=r.game_id "+whereGames, args...).Scan(&summary.TotalGames)
	db.QueryRow("SELECT COUNT(*) FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames, args...).Scan(&summary.TotalRounds)

	// Get average score and distance
	db.QueryRow("SELECT COALESCE(AVG(player_score),0) FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames, args...).Scan(&summary.AvgScore)
	db.QueryRow("SELECT COALESCE(AVG(player_dist),0) FROM rounds r JOIN games g ON g.id=r.game_id "+whereGames, args...).Scan(&summary.AvgDistance)

	// Get most confused with (where actual country is our target but player guessed elsewhere)
	confusedQuery := `SELECT country_code, COUNT(*) as count
		FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
		AND country_code != COALESCE(actual_country_code, country_code)
		AND country_code != '??'
		GROUP BY country_code ORDER BY count DESC LIMIT 1`

	var mostConfusedCode string
	var confusedCount int
	if err := db.QueryRow(confusedQuery, args...).Scan(&mostConfusedCode, &confusedCount); err == nil {
		summary.MostConfusedWith = countryCoder.NameEnByCode(mostConfusedCode)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(summary)
}

func apiCountryConfused(w http.ResponseWriter, r *http.Request) {
	// Extract country code from URL path
	path := r.URL.Path
	parts := strings.Split(path, "/")
	if len(parts) < 5 || parts[2] != "country" || parts[4] != "confused" {
		http.Error(w, "Invalid country confused path", 400)
		return
	}
	countryCode := strings.ToUpper(parts[3])

	typ := r.URL.Query().Get("type")
	mov := r.URL.Query().Get("move")
	timeline := r.URL.Query().Get("timeline")

	if typ == "" {
		typ = "standard"
	}

	// Build query conditions - use LIKE to handle compound country codes like "id|id", "ph|id", etc.
	whereGames := "WHERE game_type=? AND (COALESCE(actual_country_code, country_code) LIKE '%' || ? || '%')"
	args := []interface{}{typ, strings.ToLower(countryCode)}

	if mov != "" {
		whereGames += " AND movement=?"
		args = append(args, mov)
	}

	// Add timeline filter if specified
	if timeline != "" {
		if days, err := strconv.Atoi(timeline); err == nil && days > 0 {
			whereGames += " AND game_date >= datetime('now', '-' || ? || ' days')"
			args = append(args, days)
		}
	}

	// Find cases where actual country is our target but player guessed elsewhere
	query := `SELECT country_code, COUNT(*) as confusion_count,
		AVG(player_dist) as avg_distance_km
		FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
		AND country_code != COALESCE(actual_country_code, country_code)
		AND country_code != '??'
		GROUP BY country_code
		HAVING confusion_count >= 1
		ORDER BY confusion_count DESC LIMIT 20`

	rows, err := db.Query(query, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var confusions []CountryConfusion
	for rows.Next() {
		var guessedCode string
		var count int
		var avgDistance float64

		rows.Scan(&guessedCode, &count, &avgDistance)

		confusions = append(confusions, CountryConfusion{
			GuessedCountry: countryCoder.NameEnByCode(guessedCode),
			ActualCountry:  countryCoder.NameEnByCode(countryCode),
			Count:          count,
			AvgDistance:    avgDistance,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(confusions)
}

func apiCountryRounds(w http.ResponseWriter, r *http.Request) {
	// Extract country code from URL path
	path := r.URL.Path
	parts := strings.Split(path, "/")
	if len(parts) < 4 || parts[2] != "country" {
		http.Error(w, "Invalid country path", 400)
		return
	}
	countryCode := parts[3]

	typ := r.URL.Query().Get("type")
	mov := r.URL.Query().Get("move")
	timeline := r.URL.Query().Get("timeline")

	if typ == "" {
		typ = "standard"
	}

	// Build query conditions - use LIKE to handle compound country codes like "id|id", "ph|id", etc.
	whereGames := "WHERE game_type=? AND (COALESCE(actual_country_code, country_code) LIKE '%' || ? || '%')"
	args := []interface{}{typ, strings.ToLower(countryCode)}

	if mov != "" {
		whereGames += " AND movement=?"
		args = append(args, mov)
	}

	// Add timeline filter if specified
	if timeline != "" {
		if days, err := strconv.Atoi(timeline); err == nil && days > 0 {
			whereGames += " AND game_date >= datetime('now', '-' || ? || ' days')"
			args = append(args, days)
		}
	}

	// Query for all rounds in this country
	var query string
	if typ == "duels" {
		query = `SELECT g.id, r.round_no, r.player_score, r.opponent_score, r.player_dist,
			r.actual_lat, r.actual_lng, r.player_lat, r.player_lng, g.created, g.game_date, g.movement
			FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
			ORDER BY COALESCE(g.game_date, g.created) DESC, r.round_no ASC`
	} else {
		query = `SELECT g.id, r.round_no, r.player_score, NULL as opponent_score, r.player_dist,
			r.actual_lat, r.actual_lng, r.player_lat, r.player_lng, g.created, g.game_date, g.movement, r.round_time, r.steps_count
			FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
			ORDER BY COALESCE(g.game_date, g.created) DESC, r.round_no ASC`
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var rounds []CountryRound
	for rows.Next() {
		var round CountryRound
		var opponentScore sql.NullFloat64
		var gameDate sql.NullString
		var time sql.NullInt64
		var steps sql.NullInt64

		if typ == "duels" {
			rows.Scan(&round.GameId, &round.RoundNumber, &round.PlayerScore, &opponentScore,
				&round.Distance, &round.ActualLat, &round.ActualLng, &round.PlayerLat, &round.PlayerLng,
				&round.Created, &gameDate, &round.Movement)
		} else {
			rows.Scan(&round.GameId, &round.RoundNumber, &round.PlayerScore, &opponentScore,
				&round.Distance, &round.ActualLat, &round.ActualLng, &round.PlayerLat, &round.PlayerLng,
				&round.Created, &gameDate, &round.Movement, &time, &steps)
		}

		if opponentScore.Valid {
			round.OpponentScore = &opponentScore.Float64
		}
		if gameDate.Valid {
			round.GameDate = &gameDate.String
		}
		if time.Valid {
			timeInt := int(time.Int64)
			round.Time = &timeInt
		}
		if steps.Valid {
			stepsInt := int(steps.Int64)
			round.Steps = &stepsInt
		}

		rounds = append(rounds, round)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rounds)
}

func uiCountry(w http.ResponseWriter, r *http.Request) {
	// Extract country code from URL path
	path := r.URL.Path
	parts := strings.Split(path, "/")
	if len(parts) < 3 || parts[1] != "country" {
		http.Error(w, "Invalid country path", 400)
		return
	}
	countryCode := strings.ToUpper(parts[2])

	// Get country name
	countryName := countryCoder.NameEnByCode(countryCode)
	if countryName == "" {
		countryName = countryCode
	}

	data := struct {
		Title       string
		CountryCode string
		CountryName string
		IsPublic    bool
	}{
		Title:       countryName + " - GeoStatsr",
		CountryCode: countryCode,
		CountryName: countryName,
		IsPublic:    config.IsPublic,
	}

	w.Header().Set("Content-Type", "text/html")
	if err := templates.ExecuteTemplate(w, "country.html", data); err != nil {
		http.Error(w, err.Error(), 500)
		debugLog("Template error: %v", err)
	}
}

func apiMapData(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	mov := r.URL.Query().Get("move")

	if typ == "" {
		typ = "standard"
	}

	whereGames := "WHERE game_type=?"
	args := []interface{}{typ}
	if mov != "" {
		whereGames += " AND movement=?"
		args = append(args, mov)
	}

	query := `SELECT COALESCE(actual_country_code, country_code) as country_code,
		COUNT(*) as games,
		AVG(player_score) as avg_score,
		AVG(player_dist) as avg_distance
		FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
		GROUP BY country_code HAVING country_code != '??' AND country_code != ''
		ORDER BY games DESC`

	rows, err := db.Query(query, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var countryCode string
		var games int
		var avgScore, avgDistance float64
		rows.Scan(&countryCode, &games, &avgScore, &avgDistance)

		result = append(result, map[string]interface{}{
			"countryCode": countryCode,
			"country":     countryCoder.NameEnByCode(countryCode),
			"games":       games,
			"avgScore":    avgScore,
			"avgDistance": avgDistance,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func apiCountriesGeoJSON(w http.ResponseWriter, r *http.Request) {
	// Read the embedded countries.json file
	data, err := embeddedFS.ReadFile("countries.json")
	if err != nil {
		http.Error(w, "Failed to read countries data", 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func apiConfusedCountries(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	mov := r.URL.Query().Get("move")

	if typ == "" {
		typ = "standard"
	}

	whereGames := "WHERE game_type=?"
	args := []interface{}{typ}
	if mov != "" {
		whereGames += " AND movement=?"
		args = append(args, mov)
	}

	query := `SELECT country_code as guessed, actual_country_code as actual, COUNT(*) as count
		FROM rounds r JOIN games g ON g.id=r.game_id ` + whereGames + `
		AND country_code != '??' AND actual_country_code != '??'
		AND country_code != actual_country_code
		GROUP BY country_code, actual_country_code
		HAVING count >= 2
		ORDER BY count DESC LIMIT 20`

	rows, err := db.Query(query, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var guessed, actual string
		var count int
		rows.Scan(&guessed, &actual, &count)

		result = append(result, map[string]interface{}{
			"guessed":        guessed,
			"guessedCountry": countryCoder.NameEnByCode(guessed),
			"actual":         actual,
			"actualCountry":  countryCoder.NameEnByCode(actual),
			"count":          count,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// Serve the opponent HTML UI
func uiOpponent(w http.ResponseWriter, r *http.Request) {
	// Extract opponent ID from URL path
	path := r.URL.Path
	parts := strings.Split(path, "/")
	if len(parts) < 3 || parts[1] != "opponent" {
		http.Error(w, "Invalid opponent path", 400)
		return
	}
	opponentId := parts[2]
	opponentNick := opponentId // fallback

	// Try to get the latest known nick for this opponent from the DB
	row := db.QueryRow("SELECT opponent_nick FROM games WHERE opponent_id=? AND opponent_nick != '' ORDER BY created DESC LIMIT 1", opponentId)
	_ = row.Scan(&opponentNick)

	data := struct {
		OpponentId   string
		OpponentNick string
		IsPublic     bool
	}{
		OpponentId:   opponentId,
		OpponentNick: opponentNick,
		IsPublic:     config.IsPublic,
	}

	w.Header().Set("Content-Type", "text/html")
	if err := templates.ExecuteTemplate(w, "opponent.html", data); err != nil {
		http.Error(w, err.Error(), 500)
		debugLog("Template error: %v", err)
	}
}

// --- Opponent API endpoints ---

// /api/opponent/{id}/summary
func apiOpponentSummary(w http.ResponseWriter, r *http.Request, opponentId string) {
	move := r.URL.Query().Get("move")
	timeline := r.URL.Query().Get("timeline")

	where := "WHERE g.game_type='duels' AND g.opponent_id=?"
	args := []interface{}{opponentId}
	if move != "" {
		where += " AND g.movement=?"
		args = append(args, move)
	}
	if timeline != "" {
		where += " AND g.created >= datetime('now', ?)"
		args = append(args, "-"+timeline+" days")
	}

	var total, wins, losses, draws, daysSinceLast int
	_ = db.QueryRow("SELECT COUNT(*) FROM games g "+where, args...).Scan(&total)
	_ = db.QueryRow("SELECT COUNT(*) FROM games g "+where+" AND ((g.is_draw=0 AND g.winning_team_id=g.player_team_id))", args...).Scan(&wins)
	_ = db.QueryRow("SELECT COUNT(*) FROM games g "+where+" AND ((g.is_draw=0 AND g.winning_team_id!=g.player_team_id))", args...).Scan(&losses)
	_ = db.QueryRow("SELECT COUNT(*) FROM games g "+where+" AND g.is_draw=1", args...).Scan(&draws)
	_ = db.QueryRow("SELECT COALESCE((julianday('now') - julianday(MAX(g.created))),0) FROM games g "+where, args...).Scan(&daysSinceLast)

	winRate := 0
	if total > 0 {
		winRate = int(float64(wins) / float64(total) * 100)
	}

	resp := map[string]any{
		"totalMatches":       total,
		"wins":               wins,
		"losses":             losses,
		"draws":              draws,
		"winRate":            winRate,
		"daysSinceLastMatch": daysSinceLast,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// /api/opponent/{id}/matches
func apiOpponentMatches(w http.ResponseWriter, r *http.Request, opponentId string) {
	move := r.URL.Query().Get("move")
	timeline := r.URL.Query().Get("timeline")

	where := "WHERE g.game_type='duels' AND g.opponent_id=?"
	args := []interface{}{opponentId}
	if move != "" {
		where += " AND g.movement=?"
		args = append(args, move)
	}
	if timeline != "" {
		where += " AND g.created >= datetime('now', ?)"
		args = append(args, "-"+timeline+" days")
	}

	rows, err := db.Query(`
			SELECT g.id, g.created, g.game_date, g.movement,
				CASE
					WHEN g.is_draw = 1 THEN 'draw'
					WHEN g.winning_team_id IS NOT NULL AND g.player_team_id IS NOT NULL THEN
						CASE WHEN g.winning_team_id = g.player_team_id THEN 'win' ELSE 'loss' END
					ELSE 'unknown'
				END as result,
				COALESCE(SUM(r.player_score), 0) as yourScore,
				COALESCE(SUM(r.opponent_score), 0) as opponentScore,
				GROUP_CONCAT(DISTINCT r.actual_country_code) as countries
			FROM games g
			LEFT JOIN rounds r ON g.id = r.game_id
			`+where+`
			GROUP BY g.id, g.created, g.game_date, g.movement, g.is_draw, g.winning_team_id, g.player_team_id
			ORDER BY COALESCE(g.game_date, g.created) DESC
			LIMIT 100
		`, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var id, created, gameDate, movement, result, countries string
		var yourScore, opponentScore float64
		rows.Scan(&id, &created, &gameDate, &movement, &result, &yourScore, &opponentScore, &countries)
		out = append(out, map[string]any{
			"gameId":        id,
			"created":       created,
			"gameDate":      gameDate,
			"movement":      movement,
			"result":        result,
			"yourScore":     yourScore,
			"opponentScore": opponentScore,
			"countries":     countries,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// /api/opponent/{id}/score-comparison
func apiOpponentScoreComparison(w http.ResponseWriter, r *http.Request, opponentId string) {
	move := r.URL.Query().Get("move")
	timeline := r.URL.Query().Get("timeline")

	where := "WHERE g.game_type='duels' AND g.opponent_id=?"
	args := []interface{}{opponentId}
	if move != "" {
		where += " AND g.movement=?"
		args = append(args, move)
	}
	if timeline != "" {
		where += " AND g.created >= datetime('now', ?)"
		args = append(args, "-"+timeline+" days")
	}

	// Your stats
	var yourAvg, yourBest, yourWorst float64
	_ = db.QueryRow("SELECT COALESCE(AVG(r.player_score),0), COALESCE(MAX(r.player_score),0), COALESCE(MIN(r.player_score),0) FROM rounds r JOIN games g ON g.id=r.game_id "+where, args...).Scan(&yourAvg, &yourBest, &yourWorst)
	// Opponent stats
	var oppAvg, oppBest, oppWorst float64
	_ = db.QueryRow("SELECT COALESCE(AVG(r.opponent_score),0), COALESCE(MAX(r.opponent_score),0), COALESCE(MIN(r.opponent_score),0) FROM rounds r JOIN games g ON g.id=r.game_id "+where, args...).Scan(&oppAvg, &oppBest, &oppWorst)

	resp := map[string]any{
		"yourAvg":       int(yourAvg),
		"yourBest":      int(yourBest),
		"yourWorst":     int(yourWorst),
		"opponentAvg":   int(oppAvg),
		"opponentBest":  int(oppBest),
		"opponentWorst": int(oppWorst),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// /api/opponent/{id}/countries
func apiOpponentCountries(w http.ResponseWriter, r *http.Request, opponentId string) {
	move := r.URL.Query().Get("move")
	timeline := r.URL.Query().Get("timeline")

	where := "WHERE g.game_type='duels' AND g.opponent_id=?"
	args := []interface{}{opponentId}
	if move != "" {
		where += " AND g.movement=?"
		args = append(args, move)
	}
	if timeline != "" {
		where += " AND g.created >= datetime('now', ?)"
		args = append(args, "-"+timeline+" days")
	}

	rows, err := db.Query(`
			SELECT COALESCE(r.actual_country_code, r.country_code) as country, COUNT(*) as count
			FROM rounds r JOIN games g ON g.id=r.game_id
			`+where+`
			GROUP BY country
			ORDER BY count DESC
			LIMIT 10
		`, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var countryCode string
		var count int
		rows.Scan(&countryCode, &count)
		out = append(out, map[string]any{
			"country": countryCoder.NameEnByCode(countryCode),
			"count":   count,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// /api/opponent/{id}/performance
func apiOpponentPerformance(w http.ResponseWriter, r *http.Request, opponentId string) {
	move := r.URL.Query().Get("move")
	timeline := r.URL.Query().Get("timeline")

	where := "WHERE g.game_type='duels' AND g.opponent_id=?"
	args := []interface{}{opponentId}
	if move != "" {
		where += " AND g.movement=?"
		args = append(args, move)
	}
	if timeline != "" {
		where += " AND g.created >= datetime('now', ?)"
		args = append(args, "-"+timeline+" days")
	}

	rows, err := db.Query(`
			SELECT COALESCE(g.game_date, g.created) as date,
				SUM(r.player_score) as yourScore,
				SUM(r.opponent_score) as opponentScore
			FROM games g
			LEFT JOIN rounds r ON g.id = r.game_id
			`+where+`
			GROUP BY g.id, date
			ORDER BY date ASC
			LIMIT 100
		`, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var date string
		var yourScore, opponentScore float64
		rows.Scan(&date, &yourScore, &opponentScore)
		out = append(out, map[string]any{
			"date":          date,
			"yourScore":     yourScore,
			"opponentScore": opponentScore,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ------------------------------------------------------------
// UI endpoints

func uiIndex(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Title    string
		IsPublic bool
	}{
		Title:    "GeoStatsr",
		IsPublic: config.IsPublic,
	}

	w.Header().Set("Content-Type", "text/html")
	if err := templates.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), 500)
		debugLog("Template error: %v", err)
	}
}

func uiStatsRow(w http.ResponseWriter, r *http.Request) {
	style := r.URL.Query().Get("style")
	if style == "" {
		style = "geostatsr"
	}
	slant := r.URL.Query().Get("slant")
	if slant == "" {
		slant = "slant-left"
	}
	cards := r.URL.Query().Get("cards")

	data := map[string]interface{}{
		"Title": "Stats Row - GeoStatsr",
		"Style": style,
		"Slant": slant,
		"Cards": cards,
	}

	w.Header().Set("Content-Type", "text/html")
	if err := templates.ExecuteTemplate(w, "stats_row.html", data); err != nil {
		http.Error(w, err.Error(), 500)
		debugLog("Template error: %v", err)
	}
}

// ------------------------------------------------------------
// Periodic task management

// startPeriodicTasks starts background goroutines for periodic update checks and data collection
func startPeriodicTasks() {
	debugLog("Starting periodic tasks...")

	// Start update checker (every 24 hours)
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				debugLog("Running periodic update check...")
				checkAndPerformUpdate(true) // Always check for updates in periodic mode
			}
		}
	}()

	// Start data collector (every 6 hours)
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				debugLog("Running periodic data collection...")
				performPeriodicCollection()
			}
		}
	}()

	if logger != nil {
		logger.Info("Periodic tasks started: update check every 24h, data collection every 6h")
	} else {
		log.Println("Periodic tasks started: update check every 24h, data collection every 6h")
	}
}

// performPeriodicCollection performs the same data collection as the API endpoint
func performPeriodicCollection() {
	// Check if NCFA is set
	if config.NCFA == "" {
		debugLog("Skipping periodic collection - NCFA cookie not set")
		return
	}

	debugLog("Starting periodic collection...")
	ci := loadCountries()

	// First, collect user profile data
	debugLog("Collecting user profile data...")
	if err := collectUserProfile(); err != nil {
		debugLog("Warning: Failed to collect user profile data: %v", err)
		// Continue with game collection even if profile collection fails
	}

	debugLog("Starting pullFeed...")
	std, duels := pullFeed()
	debugLog("pullFeed returned: %d standard games, %d duels games", len(std), len(duels))

	// Track successful imports by checking if games existed before
	stdSuccess := 0
	duelsSuccess := 0

	debugLog("Starting to store standard games...")
	for i, g := range std {
		debugLog("Storing standard game %d/%d: %s", i+1, len(std), g)
		existed := rowExists(`SELECT 1 FROM rounds WHERE game_id=? LIMIT 1`, g)
		storeStandard(g, ci)
		if !existed && rowExists(`SELECT 1 FROM rounds WHERE game_id=? LIMIT 1`, g) {
			stdSuccess++
		}
	}

	debugLog("Starting to store duels games...")
	for i, d := range duels {
		debugLog("Storing duels game %d/%d: %s", i+1, len(duels), d)
		existed := rowExists(`SELECT 1 FROM rounds WHERE game_id=? LIMIT 1`, d)
		storeDuels(d, ci)
		if !existed && rowExists(`SELECT 1 FROM rounds WHERE game_id=? LIMIT 1`, d) {
			duelsSuccess++
		}
	}

	if logger != nil {
		logger.Infof("Periodic collection completed: %d new games (%d singleplayer, %d duels)",
			stdSuccess+duelsSuccess, stdSuccess, duelsSuccess)
	} else {
		log.Printf("Periodic collection completed: %d new games (%d singleplayer, %d duels)",
			stdSuccess+duelsSuccess, stdSuccess, duelsSuccess)
	}
}

// ------------------------------------------------------------

func main() {
	// Parse command line flags
	var serviceAction string
	var autoUpdate bool
	pflag.StringVarP(&configDir, "config", "c", "./", "Path to configuration directory")
	pflag.StringVarP(&serviceAction, "service", "s", "", "Service action: install, uninstall, start, stop, restart")
	pflag.BoolVar(&autoUpdate, "auto-update", true, "Enable automatic self-update")
	pflag.Parse()

	// Load configuration first
	var err error
	config, err = loadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Setup debug logging
	if config.Debug && config.LogDir != "" {
		if err := os.MkdirAll(config.LogDir, 0755); err != nil {
			log.Printf("Warning: Could not create log directory %s: %v", config.LogDir, err)
		}
	}

	debugLog("Starting GeoStatsr v%s with config: %+v", currentVersion, config)

	// Check for updates before starting the service (only if not running a service command)
	if serviceAction == "" {
		checkAndPerformUpdate(autoUpdate)
	}

	// Get the directory where the executable is located for service installation
	executablePath, err := os.Executable()
	if err != nil {
		log.Fatalf("Failed to get executable path: %v", err)
	}
	executableDir := filepath.Dir(executablePath)

	// Service configuration
	svcConfig := &service.Config{
		Name:        "GeoStatsr",
		DisplayName: "GeoStatsr - GeoGuessr Statistics Server",
		Description: "A web service that collects and displays GeoGuessr game statistics",
		Arguments:   []string{"-c", executableDir},
	}

	// Create service
	prg := &geoStatsrService{}
	svc, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatalf("Failed to create service: %v", err)
	}

	// Setup logger
	logger, err = svc.Logger(nil)
	if err != nil {
		log.Printf("Warning: Failed to create service logger: %v", err)
	}

	// Handle service actions
	if serviceAction != "" {
		switch serviceAction {
		case "install":
			err = svc.Install()
			if err != nil {
				log.Fatalf("Failed to install service: %v", err)
			}
			fmt.Println("Service installed successfully")
			return
		case "uninstall":
			err = svc.Uninstall()
			if err != nil {
				log.Fatalf("Failed to uninstall service: %v", err)
			}
			fmt.Println("Service uninstalled successfully")
			return
		case "start":
			err = svc.Start()
			if err != nil {
				log.Fatalf("Failed to start service: %v", err)
			}
			fmt.Println("Service started successfully")
			return
		case "stop":
			err = svc.Stop()
			if err != nil {
				log.Fatalf("Failed to stop service: %v", err)
			}
			fmt.Println("Service stopped successfully")
			return
		case "restart":
			err = svc.Restart()
			if err != nil {
				log.Fatalf("Failed to restart service: %v", err)
			}
			fmt.Println("Service restarted successfully")
			return
		default:
			log.Fatalf("Unknown service action: %s. Valid actions: install, uninstall, start, stop, restart", serviceAction)
		}
	}

	// Run as service or standalone
	err = svc.Run()
	if err != nil {
		// If running as service fails, try running standalone
		if logger != nil {
			logger.Info("Running in standalone mode")
		} else {
			log.Println("Running in standalone mode")
		}

		// Setup debug logging for standalone mode
		if config.Debug && config.LogDir != "" {
			if err := os.MkdirAll(config.LogDir, 0755); err != nil {
				log.Printf("Warning: Could not create log directory %s: %v", config.LogDir, err)
			}
		}

		debugLog("Starting GeoStatsr with config: %+v", config)

		initDB()
		initTemplates()
		countryCoder = NewCountryCoder(configDir) // Initialize global country coder
		mux := http.NewServeMux()
		mux.HandleFunc("/api/update_ncfa", apiUpdateCookie)
		mux.HandleFunc("/api/collect_now", apiCollectNow)
		mux.HandleFunc("/api/summary", apiSummary)
		mux.HandleFunc("/api/games", apiGames)
		mux.HandleFunc("/api/game", apiGame)
		mux.HandleFunc("/api/game_map_data", apiGameMapData)
		mux.HandleFunc("/api/country_stats", apiCountryStats)
		mux.HandleFunc("/api/chart_data", apiChartData)
		mux.HandleFunc("/api/map_data", apiMapData)
		mux.HandleFunc("/api/countries_geojson", apiCountriesGeoJSON)
		mux.HandleFunc("/api/confused_countries", apiConfusedCountries)
		// Country-specific routes
		mux.HandleFunc("/api/country/", func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if strings.HasSuffix(path, "/summary") {
				apiCountrySummary(w, r)
			} else if strings.HasSuffix(path, "/confused") {
				apiCountryConfused(w, r)
			} else if strings.HasSuffix(path, "/rounds") {
				apiCountryRounds(w, r)
			} else {
				http.NotFound(w, r)
			}
		})
		mux.HandleFunc("/country/", uiCountry)
		// Static file handler with proper MIME types
		fs := http.FileServer(http.Dir("static"))
		mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
			// Set proper MIME types based on file extension
			path := r.URL.Path
			switch {
			case strings.HasSuffix(path, ".css"):
				w.Header().Set("Content-Type", "text/css")
			case strings.HasSuffix(path, ".js"):
				w.Header().Set("Content-Type", "text/javascript")
			case strings.HasSuffix(path, ".json"):
				w.Header().Set("Content-Type", "application/json")
			case strings.HasSuffix(path, ".png"):
				w.Header().Set("Content-Type", "image/png")
			case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
				w.Header().Set("Content-Type", "image/jpeg")
			case strings.HasSuffix(path, ".gif"):
				w.Header().Set("Content-Type", "image/gif")
			case strings.HasSuffix(path, ".svg"):
				w.Header().Set("Content-Type", "image/svg+xml")
			case strings.HasSuffix(path, ".webp"):
				w.Header().Set("Content-Type", "image/webp")
			case strings.HasSuffix(path, ".woff2"):
				w.Header().Set("Content-Type", "font/woff2")
			case strings.HasSuffix(path, ".woff"):
				w.Header().Set("Content-Type", "font/woff")
			case strings.HasSuffix(path, ".ico"):
				w.Header().Set("Content-Type", "image/x-icon")
			}

			// Remove the /static/ prefix and serve the file
			http.StripPrefix("/static/", fs).ServeHTTP(w, r)
		})
		mux.HandleFunc("/stats_row", uiStatsRow)
		mux.HandleFunc("/", uiIndex)

		listenAddr := fmt.Sprintf("%s:%d", config.ListenIP, config.Port)
		log.Printf("Server starting on %s – open http://localhost:%d/", listenAddr, config.Port)
		if config.IsPublic {
			log.Printf("Running in PUBLIC mode - API updates require private key: %s", config.PrivateKey)
		} else {
			log.Printf("Running in PRIVATE mode - API updates do not require authentication")
		}
		if config.NCFA == "" {
			log.Printf("WARNING: NCFA cookie not set. Use /api/update_ncfa?token=YOUR_COOKIE to set it.")
		}

		log.Fatal(http.ListenAndServe(listenAddr, mux))
	}
}

// Helper function to normalize game dates to ISO format
func normalizeGameDate(dateInput string) string {
	if dateInput == "" {
		return ""
	}

	// Check if it's already an ISO timestamp (contains 'T' and 'Z' or '+')
	if strings.Contains(dateInput, "T") && (strings.Contains(dateInput, "Z") || strings.Contains(dateInput, "+")) {
		return dateInput
	}

	// Try to parse as Unix timestamp (epoch time)
	if timestamp, err := strconv.ParseInt(dateInput, 10, 64); err == nil {
		// Convert Unix timestamp to ISO format
		return time.Unix(timestamp/1000, (timestamp%1000)*1000000).UTC().Format(time.RFC3339)
	}

	// If it's already a string, try to parse it
	if t, err := time.Parse(time.RFC3339, dateInput); err == nil {
		return t.Format(time.RFC3339)
	}

	// If all else fails, return as-is
	return dateInput
}

// Profile data structures
type UserProfile struct {
	User struct {
		Nick        string `json:"nick"`
		Type        string `json:"type"`
		IsProUser   bool   `json:"isProUser"`
		ID          string `json:"id"`
		CountryCode string `json:"countryCode"`
		BR          struct {
			Level    int `json:"level"`
			Division int `json:"division"`
		} `json:"br"`
		Progress struct {
			CompetitionMedals struct {
				Bronze   int `json:"bronze"`
				Silver   int `json:"silver"`
				Gold     int `json:"gold"`
				Platinum int `json:"platinum"`
			} `json:"competitionMedals"`
		} `json:"progress"`
		Competitive struct {
			Elo              int `json:"elo"`
			Rating           int `json:"rating"`
			LastRatingChange int `json:"lastRatingChange"`
			Division         struct {
				Type        int `json:"type"`
				StartRating int `json:"startRating"`
				EndRating   int `json:"endRating"`
			} `json:"division"`
			OnLeaderboard bool `json:"onLeaderboard"`
		} `json:"competitive"`
	} `json:"user"`
	Email string `json:"email"`
}

// Function to collect and store user profile data
func collectUserProfile() error {
	debugLog("Collecting user profile data...")

	client := apiClient()
	resp, err := client.Get(baseV3 + "/profiles")
	if err != nil {
		debugLog("Profile fetch error: %v", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		debugLog("Profile HTTP status %d", resp.StatusCode)
		return fmt.Errorf("profile API returned status %d", resp.StatusCode)
	}

	var profile UserProfile
	if err = json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		debugLog("Profile JSON decode error: %v", err)
		return err
	}

	debugLog("Profile data: nick=%s, type=%s, isProUser=%t, id=%s, countryCode=%s",
		profile.User.Nick, profile.User.Type, profile.User.IsProUser, profile.User.ID, profile.User.CountryCode)

	// Store user metadata (using key-value store for single row data)
	_, err = db.Exec(`INSERT OR REPLACE INTO user_metadata (key, value) VALUES (?, ?)`, "nick", profile.User.Nick)
	if err != nil {
		debugLog("Error storing nick: %v", err)
	}

	_, err = db.Exec(`INSERT OR REPLACE INTO user_metadata (key, value) VALUES (?, ?)`, "type", profile.User.Type)
	if err != nil {
		debugLog("Error storing type: %v", err)
	}

	_, err = db.Exec(`INSERT OR REPLACE INTO user_metadata (key, value) VALUES (?, ?)`, "isProUser", fmt.Sprintf("%t", profile.User.IsProUser))
	if err != nil {
		debugLog("Error storing isProUser: %v", err)
	}

	_, err = db.Exec(`INSERT OR REPLACE INTO user_metadata (key, value) VALUES (?, ?)`, "id", profile.User.ID)
	if err != nil {
		debugLog("Error storing id: %v", err)
	}

	_, err = db.Exec(`INSERT OR REPLACE INTO user_metadata (key, value) VALUES (?, ?)`, "countryCode", profile.User.CountryCode)
	if err != nil {
		debugLog("Error storing countryCode: %v", err)
	}

	_, err = db.Exec(`INSERT OR REPLACE INTO user_metadata (key, value) VALUES (?, ?)`, "email", profile.Email)
	if err != nil {
		debugLog("Error storing email: %v", err)
	}

	// Check if BR rank data has changed before inserting
	var lastLevel, lastDivision int
	err = db.QueryRow(`SELECT level, division FROM br_rank ORDER BY recorded_at DESC LIMIT 1`).Scan(&lastLevel, &lastDivision)
	if err != nil || lastLevel != profile.User.BR.Level || lastDivision != profile.User.BR.Division {
		_, err = db.Exec(`INSERT INTO br_rank (level, division) VALUES (?, ?)`,
			profile.User.BR.Level, profile.User.BR.Division)
		if err != nil {
			debugLog("Error storing BR rank: %v", err)
		} else {
			debugLog("Stored new BR rank: level=%d, division=%d", profile.User.BR.Level, profile.User.BR.Division)
		}
	}

	// Check if competition medals have changed before inserting
	var lastBronze, lastSilver, lastGold, lastPlatinum int
	err = db.QueryRow(`SELECT bronze, silver, gold, platinum FROM competition_medals ORDER BY recorded_at DESC LIMIT 1`).Scan(&lastBronze, &lastSilver, &lastGold, &lastPlatinum)
	medals := profile.User.Progress.CompetitionMedals
	if err != nil || lastBronze != medals.Bronze || lastSilver != medals.Silver || lastGold != medals.Gold || lastPlatinum != medals.Platinum {
		_, err = db.Exec(`INSERT INTO competition_medals (bronze, silver, gold, platinum) VALUES (?, ?, ?, ?)`,
			medals.Bronze, medals.Silver, medals.Gold, medals.Platinum)
		if err != nil {
			debugLog("Error storing competition medals: %v", err)
		} else {
			debugLog("Stored new competition medals: bronze=%d, silver=%d, gold=%d, platinum=%d",
				medals.Bronze, medals.Silver, medals.Gold, medals.Platinum)
		}
	}

	// Check if competitive rank has changed before inserting
	var lastElo, lastRating, lastRatingChange, lastDivisionType, lastStartRating, lastEndRating int
	var lastOnLeaderboard bool
	err = db.QueryRow(`SELECT elo, rating, last_rating_change, division_type, division_start_rating, division_end_rating, on_leaderboard
		FROM competitive_rank ORDER BY recorded_at DESC LIMIT 1`).Scan(&lastElo, &lastRating, &lastRatingChange,
		&lastDivisionType, &lastStartRating, &lastEndRating, &lastOnLeaderboard)

	comp := profile.User.Competitive
	if err != nil || lastElo != comp.Elo || lastRating != comp.Rating || lastRatingChange != comp.LastRatingChange ||
		lastDivisionType != comp.Division.Type || lastStartRating != comp.Division.StartRating ||
		lastEndRating != comp.Division.EndRating || lastOnLeaderboard != comp.OnLeaderboard {

		_, err = db.Exec(`INSERT INTO competitive_rank (elo, rating, last_rating_change, division_type, division_start_rating, division_end_rating, on_leaderboard)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			comp.Elo, comp.Rating, comp.LastRatingChange, comp.Division.Type,
			comp.Division.StartRating, comp.Division.EndRating, comp.OnLeaderboard)
		if err != nil {
			debugLog("Error storing competitive rank: %v", err)
		} else {
			debugLog("Stored new competitive rank: elo=%d, rating=%d, division_type=%d",
				comp.Elo, comp.Rating, comp.Division.Type)
		}
	}

	debugLog("Profile data collection completed successfully")
	return nil
}
