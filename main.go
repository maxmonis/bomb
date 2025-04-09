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
	lobbyConnections = make(map[*websocket.Conn]bool)
	lobbyMutex sync.Mutex
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
		if (err != nil) {
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

	// Broadcast updated game list to lobby
	broadcastAvailableGames()

	json.NewEncoder(w).Encode(map[string]string{"gameId": gameID})
}

func handleJoinGame(w http.ResponseWriter, r *http.Request) {
    log.Println("Received join-game request")
    if r.Method != http.MethodPost {
        log.Printf("Invalid method: %s", r.Method)
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }

    var req struct {
        GameID  string `json:"gameId"`
        Name    string `json:"name"`
        Message string `json:"message"`
    }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        log.Printf("Failed to decode request body: %v", err)
        http.Error(w, "Invalid request body", http.StatusBadRequest)
        return
    }

    log.Printf("Join request decoded: gameID=%s, name=%s", req.GameID, req.Name)

    if req.GameID == "" || req.Name == "" {
        log.Println("Missing required fields")
        http.Error(w, "GameID and Name are required", http.StatusBadRequest)
        return
    }

    // First acquire gamesMutex to get the game
    log.Printf("Acquiring games mutex to find game %s", req.GameID)
    gamesMutex.Lock()
    game, exists := games[req.GameID]
    if !exists {
        gamesMutex.Unlock()
        log.Printf("Game not found: %s", req.GameID)
        http.Error(w, "Game not found", http.StatusNotFound)
        return
    }
    gamesMutex.Unlock()
    
    // Then acquire game mutex with timeout
    log.Printf("Attempting to acquire game mutex for game %s", req.GameID)
    lockAcquired := make(chan struct{})
    var lockTimeout bool
    
    go func() {
        game.mu.Lock()
        close(lockAcquired)
    }()

    select {
    case <-lockAcquired:
        log.Printf("Successfully acquired game mutex for game %s", req.GameID)
    case <-time.After(5 * time.Second):
        lockTimeout = true
        log.Printf("Timeout waiting for game mutex for game %s", req.GameID)
        http.Error(w, "Server busy, please try again", http.StatusServiceUnavailable)
        return
    }

    if lockTimeout {
        return
    }

    // Check if player name is already taken
    for _, player := range game.Players {
        if player.Name == req.Name {
            log.Printf("Player name already taken: %s", req.Name)
            game.mu.Unlock()
            http.Error(w, "Player name already taken", http.StatusConflict)
            return
        }
    }

    if game.InProgress {
        log.Println("Cannot join - game already in progress")
        game.mu.Unlock()
        http.Error(w, "Game already in progress", http.StatusBadRequest)
        return
    }

    // Add join request
    log.Printf("Adding join request for player: %s", req.Name)
    game.JoinRequests[req.Name] = JoinRequest{Name: req.Name, Message: req.Message}

    // Send success response
    log.Println("Sending success response")
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("{}"))
    
    // Release mutex before broadcasting
    game.mu.Unlock()
    
    // Broadcast updates without holding any locks
    log.Println("Broadcasting game state")
    broadcastGameState(game)
    
    log.Println("Broadcasting available games")
    broadcastAvailableGames()
    
    log.Println("Join request handling completed")
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Access-Control-Allow-Origin", "*")
    w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
    w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

    if r.Method == "OPTIONS" {
        w.WriteHeader(http.StatusOK)
        return
    }

    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Printf("WebSocket upgrade failed: %v", err)
        return
    }

    var msg struct {
        Type       string `json:"type"`
        GameID     string `json:"gameId"`
        Name       string `json:"name"`
        IsReconnect bool `json:"isReconnect"`
    }

    if err := conn.ReadJSON(&msg); err != nil {
        log.Printf("Error reading initial message: %v", err)
        conn.Close()
        return
    }

    if msg.GameID == "" {
        lobbyMutex.Lock()
        lobbyConnections[conn] = true
        lobbyMutex.Unlock()
        broadcastAvailableGames()
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

    log.Printf("Attempting to acquire game mutex in handleWebSocket for game %s", msg.GameID)
    game.mu.Lock()
    
    var player *Player
    if msg.IsReconnect {
        for _, p := range game.Players {
            if p.Name == msg.Name {
                p.Conn = conn
                player = p
                break
            }
        }
    } else if _, exists := game.JoinRequests[msg.Name]; exists {
        player = &Player{
            Name: msg.Name,
            Conn: conn,
        }
        delete(game.JoinRequests, msg.Name)
        game.Players = append(game.Players, player)
    } else if len(game.Players) == 0 {
        player = &Player{
            Name: msg.Name,
            Conn: conn,
        }
        game.CreatorID = msg.Name
        game.Players = append(game.Players, player)
    }

    if player == nil {
        game.mu.Unlock()
        conn.WriteJSON(map[string]string{
            "type": "error",
            "message": "Not authorized to join this game",
        })
        conn.Close()
        return
    }

    game.mu.Unlock()
    log.Printf("Released game mutex in handleWebSocket")

    // Broadcast state after releasing the mutex
    broadcastGameState(game)

    // Start handling the connection without holding any locks
    handleConnection(conn, game, player)
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
    case "make_selection":
        var content struct {
            Selection string `json:"selection"`
            Category  string `json:"category"`
        }
        if err := json.Unmarshal(msg.Content, &content); err != nil {
            log.Printf("Error unmarshaling selection: %v", err)
            return
        }
        
        // Acquire mutex
        game.mu.Lock()
        
        // Check conditions under lock
        canMakeSelection := game.Players[game.CurrentPlayer].Name == player.Name && game.ChallengeState == nil
        roundStarted := game.RoundStarted
        
        if canMakeSelection {
            validSelection := !roundStarted || validateSelection(game, content.Selection, content.Category)
            if validSelection {
                game.LastSelection = content.Selection
                game.LastCategory = content.Category
                game.UsedItems[content.Selection] = true
                game.CurrentPlayer = (game.CurrentPlayer + 1) % len(game.Players)
                game.RoundStarted = true
            }
            
            // Release mutex before broadcasting
            game.mu.Unlock()
            
            if validSelection {
                broadcastGameState(game)
            } else {
                player.Conn.WriteJSON(map[string]string{
                    "type": "error",
                    "message": "Invalid selection",
                })
            }
        } else {
            game.mu.Unlock()
        }

    case "start_game":
        game.mu.Lock()
        isCreator := player.Name == game.CreatorID
        inProgress := game.InProgress
        if isCreator && !inProgress {
            game.InProgress = true
            game.mu.Unlock()
            broadcastGameState(game)
        } else {
            game.mu.Unlock()
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
            game.mu.Unlock()
            broadcastGameState(game)
        } else {
            game.mu.Unlock()
        }

    case "give_up":
        game.mu.Lock()
        if game.ChallengeState != nil && game.ChallengeState.ChallengedName == player.Name {
            // Add letter to challenged player
            for i, p := range game.Players {
                if p.Name == player.Name {
                    p.Letters++
                    shouldRemove := p.Letters >= 4
                    if shouldRemove {
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
            game.mu.Unlock()
            broadcastGameState(game)
        } else {
            game.mu.Unlock()
        }

    case "admit_player":
        var content struct {
            PlayerName string `json:"playerName"`
        }
        if err := json.Unmarshal(msg.Content, &content); err != nil {
            log.Printf("Error unmarshaling admit player: %v", err)
            return
        }
        
        game.mu.Lock()
        isCreator := player.Name == game.CreatorID
        inProgress := game.InProgress
        _, requestExists := game.JoinRequests[content.PlayerName]
        
        if isCreator && !inProgress && requestExists {
            game.mu.Unlock()
            broadcastGameState(game)
        } else {
            game.mu.Unlock()
        }

    case "reject_player":
        var content struct {
            PlayerName string `json:"playerName"`
        }
        if err := json.Unmarshal(msg.Content, &content); err != nil {
            log.Printf("Error unmarshaling reject player: %v", err)
            return
        }
        
        game.mu.Lock()
        if player.Name == game.CreatorID {
            if _, exists := game.JoinRequests[content.PlayerName]; exists {
                delete(game.JoinRequests, content.PlayerName)
                game.mu.Unlock()
                broadcastGameState(game)
                return
            }
        }
        game.mu.Unlock()

    case "start_turn":
        game.mu.Lock()
        if game.Players[game.CurrentPlayer].Name == player.Name {
            game.mu.Unlock()
            startTimer(game)
        } else {
            game.mu.Unlock()
        }
    }

    // Check for game end
    game.mu.Lock()
    if len(game.Players) == 1 {
        // Get winner under lock
        winner := game.Players[0].Name
        game.mu.Unlock()
        
        // Notify winner without holding the lock
        for _, p := range game.Players {
            p.Conn.WriteJSON(map[string]string{
                "type": "game_won",
                "winner": winner,
            })
        }
        
        // Remove game from global state
        gamesMutex.Lock()
        delete(games, game.ID)
        gamesMutex.Unlock()
    } else {
        game.mu.Unlock()
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
		
		 // Broadcast updated game list to lobby
		broadcastAvailableGames()
		
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
    // Make a copy of the state under lock
    game.mu.Lock()
    state := struct {
        Type    string `json:"type"`
        Game    *Game  `json:"game"`
    }{
        Type: "game_state",
        Game: &Game{
            ID:            game.ID,
            Players:       make([]*Player, len(game.Players)),
            CreatorID:     game.CreatorID,
            InProgress:    game.InProgress,
            CurrentPlayer: game.CurrentPlayer,
            LastSelection: game.LastSelection,
            LastCategory:  game.LastCategory,
            UsedItems:     make(map[string]bool),
            ChallengeState: game.ChallengeState,
            RoundStarted:   game.RoundStarted,
            JoinRequests:   make(map[string]JoinRequest),
            TimeLeft:       game.TimeLeft,
        },
    }

    // Copy players
    copy(state.Game.Players, game.Players)
    
    // Copy maps
    for k, v := range game.UsedItems {
        state.Game.UsedItems[k] = v
    }
    for k, v := range game.JoinRequests {
        state.Game.JoinRequests[k] = v
    }

    // Get players list while under lock
    players := make([]*Player, len(game.Players))
    copy(players, game.Players)
    
    game.mu.Unlock()

    // Broadcast without holding the lock
    var failedPlayers []*Player
    for _, player := range players {
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

    // Re-acquire lock to remove failed players
    if len(failedPlayers) > 0 {
        game.mu.Lock()
        for _, failedPlayer := range failedPlayers {
            for i, p := range game.Players {
                if p == failedPlayer {
                    game.Players = append(game.Players[:i], game.Players[i+1:]...)
                    // If this was the creator, end the game
                    if failedPlayer.Name == game.CreatorID {
                        gamesMutex.Lock()
                        delete(games, game.ID)
                        gamesMutex.Unlock()
                        game.mu.Unlock()
                        return
                    }
                    break
                }
            }
        }
        game.mu.Unlock()
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

func broadcastAvailableGames() {
	gamesMutex.Lock()
	var gameList []string
	for gameID := range games {
		gameList = append(gameList, gameID)
	}
	gamesMutex.Unlock()

	message := struct {
		Type  string   `json:"type"`
		Games []string `json:"games"`
	}{
		Type:  "available_games",
		Games: gameList,
	}

	lobbyMutex.Lock()
	for conn := range lobbyConnections {
		if err := conn.WriteJSON(message); err != nil {
			conn.Close()
			delete(lobbyConnections, conn)
		}
	}
	lobbyMutex.Unlock()
}