package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Player struct {
	Name     string `json:"name"`
	Letters  int    `json:"letters"`
	IsActive bool   `json:"isActive"`
	Conn     *websocket.Conn `json:"-"`
}

type JoinRequest struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

type Game struct {
	ID            string            `json:"id"`
	Players       []*Player         `json:"players"`
	CreatorID     string           `json:"creatorId"`
	InProgress    bool             `json:"inProgress"`
	CurrentPlayer int              `json:"currentPlayer"`
	LastSelection string           `json:"lastSelection"`
	LastCategory  string           `json:"lastCategory"`
	UsedItems     map[string]bool  `json:"usedItems"`
	mu            sync.Mutex       `json:"-"`
	ChallengeState  *ChallengeState    `json:"challengeState,omitempty"`
	RoundStarted    bool               `json:"roundStarted"`
	JoinRequests    map[string]JoinRequest `json:"joinRequests"`
	Timer           *time.Timer            `json:"-"`
	TimeLeft        int                    `json:"timeLeft"`
}

type ChallengeState struct {
	ChallengerName  string `json:"challengerName"`
	ChallengedName  string `json:"challengedName"`
	ExpectedCategory string `json:"expectedCategory"`
}

type GameMessage struct {
	Type    string          `json:"type"`
	Content json.RawMessage `json:"content"`
}

var (
	games = make(map[string]*Game)
	gamesMutex sync.Mutex
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow all origins for now
		},
		// Add explicit headers for Safari
		EnableCompression: true,
		HandshakeTimeout: 10 * time.Second,
		Subprotocols: []string{"websocket"},
	}
)

func main() {
	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/create-game", handleCreateGame)
	http.HandleFunc("/join-game", handleJoinGame)
	http.HandleFunc("/get-games", handleGetGames)
	
	// Keep the search endpoint for Wikipedia API
	http.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		if query == "" {
			log.Println("Query parameter is missing")
			http.Error(w, "Query parameter is required", http.StatusBadRequest)
			return
		}

		category := r.URL.Query().Get("category")
		if category == "" {
			log.Println("Category parameter is missing")
			http.Error(w, "Category parameter is required", http.StatusBadRequest)
			return
		}

		// If validating a selection, check the relationship
        lastSelection := r.URL.Query().Get("lastSelection")
        if lastSelection != "" {
            isValid := false
            if category == "actor" {
                isValid = validateMovieActorRelation(lastSelection, query)
            } else {
                isValid = validateMovieActorRelation(query, lastSelection)
            }
            
            if (!isValid) {
                w.Header().Set("Content-Type", "application/json")
                json.NewEncoder(w).Encode([]string{})
                return
            }
        }

		encodedQuery := url.QueryEscape(query)
		searchUrl := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=opensearch&format=json&search=%s", encodedQuery)
		resp, err := http.Get(searchUrl)
		if err != nil {
			log.Printf("Failed to fetch data from Wikimedia API: %v\n", err)
			http.Error(w, "Failed to fetch data from Wikimedia API", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read response body: %v\n", err)
			http.Error(w, "Failed to read response body", http.StatusInternalServerError)
			return
		}

		if resp.StatusCode != http.StatusOK {
			log.Printf("Wikimedia API returned non-200 status: %d\nResponse body: %s\n", resp.StatusCode, string(body))
			http.Error(w, "Failed to fetch data from Wikimedia API", http.StatusInternalServerError)
			return
		}

		var data []interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			log.Printf("Failed to parse JSON response: %v\n", err)
			http.Error(w, "Failed to parse JSON response", http.StatusInternalServerError)
			return
		}

		results, ok := data[1].([]interface{})
		if !ok {
			log.Println("Unexpected response format from Wikimedia API")
			http.Error(w, "Unexpected response format", http.StatusInternalServerError)
			return
		}

		var titles []string
		for _, result := range results {
			titles = append(titles, result.(string))
		}

		// Fetch revisions for the titles
		revisionsURL := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=query&prop=revisions&rvprop=content&titles=%s&format=json", url.QueryEscape(strings.Join(titles, "|")))
		revisionsResp, err := http.Get(revisionsURL)
		if err != nil {
			log.Printf("Failed to fetch revisions from Wikimedia API: %v\n", err)
			http.Error(w, "Failed to fetch revisions from Wikimedia API", http.StatusInternalServerError)
			return
		}
		defer revisionsResp.Body.Close()

		revisionsBody, err := io.ReadAll(revisionsResp.Body)
		if err != nil {
			log.Printf("Failed to read revisions response body: %v\n", err)
			http.Error(w, "Failed to read revisions response body", http.StatusInternalServerError)
			return
		}

		if revisionsResp.StatusCode != http.StatusOK {
			log.Printf("Wikimedia API returned non-200 status for revisions: %d\nResponse body: %s\n", revisionsResp.StatusCode, string(revisionsBody))
			http.Error(w, "Failed to fetch revisions from Wikimedia API", http.StatusInternalServerError)
			return
		}

		var revisionsData map[string]interface{}
		if err := json.Unmarshal(revisionsBody, &revisionsData); err != nil {
			log.Printf("Failed to parse revisions JSON response: %v\n", err)
			http.Error(w, "Failed to parse revisions JSON response", http.StatusInternalServerError)
			return
		}

		// Combine titles and revisions into a response, filtering by "birth_date"
		filteredTitles := []string{}
		if queryPages, ok := revisionsData["query"].(map[string]interface{})["pages"].(map[string]interface{}); ok {
			for _, page := range queryPages {
				if pageMap, ok := page.(map[string]interface{}); ok {
					title, _ := pageMap["title"].(string)
					revisions, _ := pageMap["revisions"].([]interface{})
					for _, revision := range revisions {
						if revisionMap, ok := revision.(map[string]interface{}); ok {
							term := "birth_date"
							if category == "movie" {
								term = "cinematography"
							}
							if content, ok := revisionMap["*"].(string); ok && strings.Contains(content, term) {
								filteredTitles = append(filteredTitles, title)
								break
							}
						}
					}
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(filteredTitles); err != nil {
			log.Printf("Failed to encode JSON response: %v\n", err)
			http.Error(w, "Failed to encode JSON response", http.StatusInternalServerError)
		}

	})

	fmt.Println("Server running on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleCreateGame(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		CreatorName string `json:"creatorName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	gameID := uuid.New().String()
	game := &Game{
		ID:           gameID,
		Players:      make([]*Player, 0),
		UsedItems:    make(map[string]bool),
		JoinRequests: make(map[string]JoinRequest),
	}

	gamesMutex.Lock()
	games[gameID] = game
	gamesMutex.Unlock()

	json.NewEncoder(w).Encode(map[string]string{"gameId": gameID})
}

func handleJoinGame(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    var req struct {
        GameID  string `json:"gameId"`
        Name    string `json:"name"`
        Message string `json:"message"`
    }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "Invalid request body", http.StatusBadRequest)
        return
    }

    if req.GameID == "" || req.Name == "" {
        http.Error(w, "GameID and Name are required", http.StatusBadRequest)
        return
    }

    gamesMutex.Lock()
    game, exists := games[req.GameID]
    if !exists {
        gamesMutex.Unlock()
        http.Error(w, "Game not found", http.StatusNotFound)
        return
    }
    gamesMutex.Unlock()

    game.mu.Lock()
    defer game.mu.Unlock()

    // Check if player name is already taken
    for _, player := range game.Players {
        if player.Name == req.Name {
            http.Error(w, "Player name already taken", http.StatusConflict)
            return
        }
    }

    if game.InProgress {
        http.Error(w, "Game already in progress", http.StatusBadRequest)
        return
    }

    // Add join request
    game.JoinRequests[req.Name] = JoinRequest{Name: req.Name, Message: req.Message}

    // Notify creator about the new join request
    state := struct {
        Type    string `json:"type"`
        Game    *Game  `json:"game"`
    }{
        Type: "game_state",
        Game: game,
    }

    // Try to notify creator
    for _, player := range game.Players {
        if player.Name == game.CreatorID {
            if err := player.Conn.WriteJSON(state); err != nil {
                log.Printf("Failed to notify creator about new join request: %v", err)
            }
            break
        }
    }

    w.WriteHeader(http.StatusOK)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
    // Add required headers for Safari
    w.Header().Set("Access-Control-Allow-Origin", "*")
    w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
    w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

    // Handle preflight OPTIONS request
    if r.Method == "OPTIONS" {
        w.WriteHeader(http.StatusOK)
        return
    }

    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Printf("WebSocket upgrade failed: %v", err)
        return
    }

    // Read initial message
    var msg struct {
        Type     string `json:"type"`
        GameID   string `json:"gameId"`
        Name     string `json:"name"`
        IsReconnect bool `json:"isReconnect"`
    }

    if err := conn.ReadJSON(&msg); err != nil {
        log.Printf("Error reading initial message: %v", err)
        conn.Close()
        return
    }

    gamesMutex.Lock()
    game, exists := games[msg.GameID]
    gamesMutex.Unlock()

    if !exists {
        conn.WriteJSON(map[string]string{
            "type": "error",
            "message": "Game not found",
        })
        conn.Close()
        return
    }

    game.mu.Lock()
    defer game.mu.Unlock()

    // Handle reconnection
    if msg.IsReconnect {
        for _, p := range game.Players {
            if p.Name == msg.Name {
                p.Conn = conn
                // Send current game state
                broadcastGameState(game)
                go handleConnection(conn, game, p)
                return
            }
        }
    }

    // For new connections, check if player has been admitted
    if _, exists := game.JoinRequests[msg.Name]; exists {
        player := &Player{
            Name: msg.Name,
            Conn: conn,
        }

        // Remove from join requests since they're now connecting
        delete(game.JoinRequests, msg.Name)
        
        // Add to active players
        game.Players = append(game.Players, player)
        broadcastGameState(game)
        go handleConnection(conn, game, player)
    } else if len(game.Players) == 0 {
        // First player (creator) can always connect
        player := &Player{
            Name: msg.Name,
            Conn: conn,
        }
        game.CreatorID = msg.Name
        game.Players = append(game.Players, player)
        broadcastGameState(game)
        go handleConnection(conn, game, player)
    } else {
        // Player trying to connect without being admitted
        conn.WriteJSON(map[string]string{
            "type": "error",
            "message": "Not authorized to join this game",
        })
        conn.Close()
    }
}

func handleConnection(conn *websocket.Conn, game *Game, player *Player) {
	defer conn.Close()

	// Handle incoming messages
	for {
		var gameMsg GameMessage
		if err := conn.ReadJSON(&gameMsg); err != nil {
			if websocket.IsUnexpectedCloseError(err) {
				handlePlayerDisconnect(game, player)
			}
			break
		}
		handleGameMessage(game, player, gameMsg)
	}
}

func validateSelection(game *Game, selection, category string) bool {
    if category == "movie" {
        // For movies, check if the last actor was in this movie
        // This would require an additional API call to validate
        return true // TODO: Implement actual validation
    } else {
        // For actors, check if they were in the last movie
        // This would require an additional API call to validate
        return true // TODO: Implement actual validation
    }
}

func handleGameMessage(game *Game, player *Player, msg GameMessage) {
    switch msg.Type {
    // ...existing code...
    case "make_selection":
        var content struct {
            Selection string `json:"selection"`
            Category  string `json:"category"`
        }
        json.Unmarshal(msg.Content, &content)
        
        game.mu.Lock()
        // Only allow selection if it's the player's turn and no challenge is active
        if game.Players[game.CurrentPlayer].Name == player.Name && game.ChallengeState == nil {
            // Validate the selection if this isn't the first move of a round
            if !game.RoundStarted || validateSelection(game, content.Selection, content.Category) {
                game.LastSelection = content.Selection
                game.LastCategory = content.Category
                game.UsedItems[content.Selection] = true
                game.CurrentPlayer = (game.CurrentPlayer + 1) % len(game.Players)
                game.RoundStarted = true
                
                // Broadcast success
                broadcastGameState(game)
            } else {
                // Invalid selection during ongoing round
                player.Conn.WriteJSON(map[string]string{
                    "type": "error",
                    "message": "Invalid selection - connection not found",
                })
            }
        }
        game.mu.Unlock()

    // ...existing code...
    case "start_game":
		if player.Name == game.CreatorID && !game.InProgress {
			game.InProgress = true
			broadcastGameState(game)
		}
	case "challenge":
		game.mu.Lock()
		if game.RoundStarted && game.ChallengeState == nil {
			previousPlayerIndex := (game.CurrentPlayer - 1 + len(game.Players)) % len(game.Players)
			game.ChallengeState = &ChallengeState{
				ChallengerName: player.Name,
				ChallengedName: game.Players[previousPlayerIndex].Name,
				ExpectedCategory: game.LastCategory,
			}
			game.CurrentPlayer = previousPlayerIndex
		}
		game.mu.Unlock()
		broadcastGameState(game)

	case "give_up":
		game.mu.Lock()
		if game.ChallengeState != nil && game.ChallengeState.ChallengedName == player.Name {
			// Add letter to challenged player
			for i, p := range game.Players {
				if p.Name == player.Name {
					p.Letters++
					if p.Letters >= 4 {
						// Remove eliminated player
						game.Players = append(game.Players[:i], game.Players[i+1:]...)
					}
					break
				}
			}
			// Reset round
			game.ChallengeState = nil
			game.LastSelection = ""
			game.LastCategory = ""
			game.RoundStarted = false
		}
		game.mu.Unlock()
		broadcastGameState(game)

	case "challenge_success":
		game.mu.Lock()
		if game.ChallengeState != nil {
			// Add letter to challenger
			for i, p := range game.Players {
				if p.Name == game.ChallengeState.ChallengerName {
					p.Letters++
					if p.Letters >= 4 {
						// Remove eliminated player
						game.Players = append(game.Players[:i], game.Players[i+1:]...)
					}
					break
				}
			}
			// Reset round
			game.ChallengeState = nil
			game.LastSelection = ""
			game.LastCategory = ""
			game.RoundStarted = false
		}
		game.mu.Unlock()
		broadcastGameState(game)

	case "admit_player":
        var content struct {
            PlayerName string `json:"playerName"`
        }
        json.Unmarshal(msg.Content, &content)
        
        game.mu.Lock()
        if player.Name == game.CreatorID && !game.InProgress {
            // Only delete from JoinRequests if the player hasn't connected yet
            if _, exists := game.JoinRequests[content.PlayerName]; exists {
                // Leave request in place until player connects via WebSocket
                // The actual player addition happens in handleWebSocket
                // Just notify the waiting player that they've been admitted
                broadcastGameState(game)
            }
        }
        game.mu.Unlock()

    case "reject_player":
        var content struct {
            PlayerName string `json:"playerName"`
        }
        json.Unmarshal(msg.Content, &content)
        
        game.mu.Lock()
        if player.Name == game.CreatorID {
            if _, exists := game.JoinRequests[content.PlayerName]; exists {
                delete(game.JoinRequests, content.PlayerName)
                // Notify the rejected player
                broadcastGameState(game)
            }
        }
        game.mu.Unlock()

    case "start_turn":
        game.mu.Lock()
        if game.Players[game.CurrentPlayer].Name == player.Name {
            startTimer(game)
        }
        game.mu.Unlock()
    }

	// Check for game end
	if len(game.Players) == 1 {
		// Notify winner
		for _, p := range game.Players {
			p.Conn.WriteJSON(map[string]string{
				"type": "game_won",
				"winner": p.Name,
			})
		}
		// Remove game
		gamesMutex.Lock()
		delete(games, game.ID)
		gamesMutex.Unlock()
	}
}

func startTimer(game *Game) {
    if game.Timer != nil {
        game.Timer.Stop()
    }
    
    game.TimeLeft = 30
    broadcastGameState(game)

    game.Timer = time.NewTimer(time.Second)
    go func() {
        for {
            <-game.Timer.C
            game.mu.Lock()
            game.TimeLeft--
            
            if game.TimeLeft <= 0 {
                // Time's up - current player gets a letter
                for i, p := range game.Players {
                    if i == game.CurrentPlayer {
                        p.Letters++
                        if p.Letters >= 4 {
                            game.Players = append(game.Players[:i], game.Players[i+1:]...)
                        }
                        break
                    }
                }
                
                // Start new round
                game.LastSelection = ""
                game.LastCategory = ""
                game.RoundStarted = false
                game.CurrentPlayer = (game.CurrentPlayer + 1) % len(game.Players)
                game.Timer = nil
                broadcastGameState(game)
                game.mu.Unlock()
                return
            }
            
            game.Timer.Reset(time.Second)
            broadcastGameState(game)
            game.mu.Unlock()
        }
    }()
}

func handlePlayerDisconnect(game *Game, player *Player) {
	game.mu.Lock()
	defer game.mu.Unlock()

	// Remove player from the game
	for i, p := range game.Players {
		if p == player {
			game.Players = append(game.Players[:i], game.Players[i+1:]...)
			break
		}
	}

	// If creator disconnects, end the game
	if player.Name == game.CreatorID {
		gamesMutex.Lock()
		delete(games, game.ID)
		gamesMutex.Unlock()
		
		// Notify remaining players
		for _, p := range game.Players {
			p.Conn.WriteJSON(map[string]string{
				"type": "game_ended",
				"message": "Game creator disconnected",
			})
		}
		return
	}

	broadcastGameState(game)
}

func broadcastGameState(game *Game) {
	game.mu.Lock()
	defer game.mu.Unlock()

	state := struct {
		Type    string `json:"type"`
		Game    *Game  `json:"game"`
	}{
		Type: "game_state",
		Game: game,
	}

	var failedPlayers []*Player
	for _, player := range game.Players {
		// Try to send the state up to 3 times
		var err error
		for attempts := 0; attempts < 3; attempts++ {
			if err = player.Conn.WriteJSON(state); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			log.Printf("Error broadcasting to player %s after retries: %v", player.Name, err)
			failedPlayers = append(failedPlayers, player)
		}
	}

	// Remove players who couldn't receive the update
	for _, failedPlayer := range failedPlayers {
		for i, p := range game.Players {
			if p == failedPlayer {
				game.Players = append(game.Players[:i], game.Players[i+1:]...)
				// If this was the creator, end the game
				if failedPlayer.Name == game.CreatorID {
					gamesMutex.Lock()
					delete(games, game.ID)
					gamesMutex.Unlock()
					return
				}
				break
			}
		}
	}
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, "index.html")
}

func validateMovieActorRelation(movieTitle, actorName string) bool {
    // Check movie page for actor
    if foundInMovie := checkMoviePage(movieTitle, actorName); foundInMovie {
        return true
    }
    
    // Check actor page for movie
    return checkActorPage(actorName, movieTitle)
}

func checkMoviePage(movieTitle, actorName string) bool {
    movieURL := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=query&prop=revisions&rvprop=content&format=json&titles=%s", url.QueryEscape(movieTitle))
    movieResp, err := http.Get(movieURL)
    if err != nil {
        log.Printf("Error fetching movie data: %v", err)
        return false
    }
    defer movieResp.Body.Close()

    var movieData map[string]interface{}
    if err := json.NewDecoder(movieResp.Body).Decode(&movieData); err != nil {
        log.Printf("Error decoding movie data: %v", err)
        return false
    }

    content := extractWikiContent(movieData)
    return searchForConnection(content, actorName, []string{"starring", "cast"})
}

func checkActorPage(actorName, movieTitle string) bool {
    actorURL := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=query&prop=revisions&rvprop=content&format=json&titles=%s", url.QueryEscape(actorName))
    actorResp, err := http.Get(actorURL)
    if err != nil {
        log.Printf("Error fetching actor data: %v", err)
        return false
    }
    defer actorResp.Body.Close()

    var actorData map[string]interface{}
    if err := json.NewDecoder(actorResp.Body).Decode(&actorData); err != nil {
        log.Printf("Error decoding actor data: %v", err)
        return false
    }

    content := extractWikiContent(actorData)
    return searchForConnection(content, movieTitle, []string{"filmography", "films", "movies"})
}

func extractWikiContent(data map[string]interface{}) string {
    if query, ok := data["query"].(map[string]interface{}); ok {
        if pages, ok := query["pages"].(map[string]interface{}); ok {
            for _, page := range pages {
                if pagemap, ok := page.(map[string]interface{}); ok {
                    if revisions, ok := pagemap["revisions"].([]interface{}); ok && len(revisions) > 0 {
                        if revision, ok := revisions[0].(map[string]interface{}); ok {
                            if content, ok := revision["*"].(string); ok {
                                return content
                            }
                        }
                    }
                }
            }
        }
    }
    return ""
}

func searchForConnection(content, searchTerm string, sections []string) bool {
    content = strings.ToLower(content)
    searchTerm = strings.ToLower(searchTerm)
    
    for _, section := range sections {
        if idx := strings.Index(content, section); idx != -1 {
            sectionContent := content[idx:]
            if endIdx := strings.Index(sectionContent, "=="); endIdx != -1 {
                sectionContent = sectionContent[:endIdx]
            }
            if strings.Contains(sectionContent, searchTerm) {
                return true
            }
        }
    }
    return false
}

func handleGetGames(w http.ResponseWriter, r *http.Request) {
	gamesMutex.Lock()
	defer gamesMutex.Unlock()

	var gameList []string
	for gameID := range games {
		gameList = append(gameList, gameID)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(gameList)
}