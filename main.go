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

	"github.com/gorilla/websocket"
)

type Player struct {
	Name     string `json:"name"`
	Letters  int    `json:"letters"`
	IsActive bool   `json:"isActive"`
	IsEliminated bool `json:"isEliminated"`
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
	Mu            sync.Mutex       `json:"-"`
	ChallengeState  *ChallengeState    `json:"challengeState,omitempty"`
	RoundStarted    bool               `json:"roundStarted"`
	JoinRequests    map[string]JoinRequest `json:"joinRequests"`
	Timer           *time.Timer            `json:"-"`
	TimeLeft        int                    `json:"timeLeft"`
	PendingConns    map[string]*websocket.Conn `json:"-"`
	SelectionHistory []string          `json:"selectionHistory"`
	SelectionRounds  [][]string       `json:"selectionRounds"`
	IsNewRound      bool              `json:"isNewRound"`
}

type ChallengeState struct {
	ChallengerName   string `json:"challengerName"`
	ChallengedName   string `json:"challengedName"`
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

func broadcastAvailableGames() {
	gamesMutex.Lock()
	var gameList []string
	for gameID, game := range games {
		// Only include games that are not in progress
		if (!game.InProgress) {
			gameList = append(gameList, gameID)
		}
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

func broadcastGameState(game *Game) {
    // Make a copy of the state under lock
    game.Mu.Lock()
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
            SelectionHistory: game.SelectionHistory,
            SelectionRounds: game.SelectionRounds,
            IsNewRound: game.IsNewRound,
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

    // Get all connections that need updating while under lock
    connections := make(map[*websocket.Conn]bool)
    for _, p := range game.Players {
        if p.Conn != nil {
            connections[p.Conn] = true
        }
    }
    for _, conn := range game.PendingConns {
        if conn != nil {
            connections[conn] = true
        }
    }
    
    game.Mu.Unlock()

    // Broadcast without holding the lock
    var failedConns []*websocket.Conn
    for conn := range connections {
        var err error
        for attempts := 0; attempts < 3; attempts++ {
            if err = conn.WriteJSON(state); err == nil {
                break
            }
            time.Sleep(100 * time.Millisecond)
        }
        if err != nil {
            failedConns = append(failedConns, conn)
        }
    }

    // Handle failed connections
    if len(failedConns) > 0 {
        game.Mu.Lock()
        for _, failedConn := range failedConns {
            // Find and mark player as inactive, but don't remove them
            for _, p := range game.Players {
                if p.Conn == failedConn {
                    p.IsActive = false
                    p.Conn = nil
                    // If this was the creator, end the game
                    if p.Name == game.CreatorID {
                        gamesMutex.Lock()
                        delete(games, game.ID)
                        gamesMutex.Unlock()
                        game.Mu.Unlock()
                        return
                    }
                    break
                }
            }
            // Remove from pending connections if present
            for name, conn := range game.PendingConns {
                if conn == failedConn {
                    delete(game.PendingConns, name)
                    delete(game.JoinRequests, name)
                    break
                }
            }
        }
        game.Mu.Unlock()
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

    // Get current time in milliseconds since Unix epoch
    timestamp := time.Now().UnixMilli()
    
    // Convert to string
    tsString := fmt.Sprintf("%d", timestamp)
    
    // Generate game ID using creator name and last 8 digits of current timestamp
    gameID := fmt.Sprintf("%s-%s", req.CreatorName, tsString[len(tsString)-8:])

    game := &Game{
        ID:           gameID,
        CreatorID:    req.CreatorName,
        Players:      make([]*Player, 0),
        UsedItems:    make(map[string]bool),
        JoinRequests: make(map[string]JoinRequest),
        PendingConns: make(map[string]*websocket.Conn),
        SelectionHistory: make([]string, 0),
        SelectionRounds: make([][]string, 0),
    }

    gamesMutex.Lock()
    games[gameID] = game
    gamesMutex.Unlock()

    // Broadcast updated game list to lobby
    broadcastAvailableGames()

    json.NewEncoder(w).Encode(map[string]string{"gameId": gameID})
}

func handleGameEnded(game *Game, message string) {
    // Stop the timer
    game.Timer.Stop()
    game.Mu.Lock()
    // Make a copy of players to notify while holding the lock
    players := make([]*Player, len(game.Players))
    copy(players, game.Players)
    
    // Notify all players
    for _, p := range players {
        if p.Conn != nil {
            p.Conn.WriteJSON(map[string]string{
                "type": "winner_announced",
                "message": message,
            })
        }
    }
}

func handleGameMessage(game *Game, player *Player, msg GameMessage) {
    switch msg.Type {
    case "admit_player":
        var content struct {
            PlayerName string `json:"playerName"`
        }
        if err := json.Unmarshal(msg.Content, &content); err != nil {
            log.Printf("Error unmarshaling admit player: %v", err)
            return
        }
        
        game.Mu.Lock()
        isCreator := player.Name == game.CreatorID
        inProgress := game.InProgress
        _, requestExists := game.JoinRequests[content.PlayerName]
        
        if isCreator && !inProgress && requestExists {
            // Get the pending connection
            pendingConn := game.PendingConns[content.PlayerName]
            
            // Add the player to the game
            game.Players = append(game.Players, &Player{
                Name: content.PlayerName,
                IsActive: true,
                Conn: pendingConn,
            })
            
            // Clean up
            delete(game.PendingConns, content.PlayerName)
            delete(game.JoinRequests, content.PlayerName)
            
            game.Mu.Unlock()
            broadcastGameState(game)
            
            // Notify the admitted player
            if pendingConn != nil {
                pendingConn.WriteJSON(map[string]string{
                    "type": "join_accepted",
                    "message": "You have been admitted to the game",
                })
            }
        } else {
            game.Mu.Unlock()
        }

    case "challenge":
        game.Mu.Lock()
        if game.RoundStarted && game.ChallengeState == nil {
            // Find the previous non-eliminated player
            previousPlayerIndex := game.CurrentPlayer
            for i := 0; i < len(game.Players); i++ {
                previousPlayerIndex = (previousPlayerIndex - 1 + len(game.Players)) % len(game.Players)
                if !game.Players[previousPlayerIndex].IsEliminated {
                    break
                }
            }
            previousPlayer := game.Players[previousPlayerIndex]
            
            // Only proceed with challenge if we found a valid previous player that isn't eliminated
            if !previousPlayer.IsEliminated {
                // Set up challenge state
                game.ChallengeState = &ChallengeState{
                    ChallengerName: player.Name,
                    ChallengedName: previousPlayer.Name,
                    ExpectedCategory: game.LastCategory,
                }
                
                // Give control back to the challenged player
                game.CurrentPlayer = previousPlayerIndex
                game.TimeLeft = 30 // Reset timer for challenged player
                
                game.Mu.Unlock()
                broadcastGameState(game)
                startTimer(game) // Start timer for challenged player
                return
            }
        }
        game.Mu.Unlock()

    case "delete_game":
        gamesMutex.Lock()
        delete(games, game.ID)
        gamesMutex.Unlock()
        broadcastAvailableGames()
        // broadcast ID of deleted game to all players
        for _, p := range game.Players {
            if p.Conn != nil {
                p.Conn.WriteJSON(map[string]string{
                    "type": "game_deleted",
                    "gameId": game.ID,
                })
            }
        }

    case "give_up":
        game.Mu.Lock()
        if game.ChallengeState != nil && game.ChallengeState.ChallengedName == player.Name {
            // Add letter to challenged player
            var activePlayers int
            var winner string
            for _, p := range game.Players {
                if p.Name == player.Name {
                    p.Letters++
                    // Start a new round since a player got a letter
                    if len(game.SelectionHistory) > 0 {
                        game.SelectionRounds = append(game.SelectionRounds, game.SelectionHistory)
                    }
                    game.SelectionHistory = make([]string, 0)
                    game.IsNewRound = true
                    if p.Letters >= 4 {
                        // Mark player as eliminated
                        p.IsEliminated = true
                        // Send elimination notification
                        if p.Conn != nil {
                            p.Conn.WriteJSON(map[string]string{
                                "type": "player_eliminated",
                                "message": "You've been eliminated, better luck next time!",
                            })
                        }
                    }
                    break
                }
            }

            // Count active players and find potential winner
            for _, p := range game.Players {
                if !p.IsEliminated {
                    activePlayers++
                    winner = p.Name
                }
            }

            // If only one player remains active, they win
            if activePlayers == 1 {
                game.Mu.Unlock()
                handleGameEnded(game, fmt.Sprintf("%s wins!", winner))
                return
            }
            
            // Reset game state for next round
            game.ChallengeState = nil
            game.LastSelection = ""
            game.LastCategory = ""
            game.RoundStarted = false
            game.TimeLeft = 30

            // Find next active player
            nextPlayer := (game.CurrentPlayer + 1) % len(game.Players)
            for game.Players[nextPlayer].IsEliminated {
                nextPlayer = (nextPlayer + 1) % len(game.Players)
            }
            game.CurrentPlayer = nextPlayer
            
            game.Mu.Unlock()
            broadcastGameState(game)
            startTimer(game)
        } else {
            game.Mu.Unlock()
        }

    case "make_selection":
        var content struct {
            Selection string `json:"selection"`
            Category  string `json:"category"`
        }
        if err := json.Unmarshal(msg.Content, &content); err != nil {
            log.Printf("Error unmarshaling selection: %v", err)
            return
        }
        
        game.Mu.Lock()
        
        // Check if this is a challenge response or normal turn
        if game.ChallengeState != nil && game.ChallengeState.ChallengedName == player.Name {
            // Challenge failed, add letter to challenger and add valid selection to history
            game.SelectionHistory = append(game.SelectionHistory, content.Selection)
            game.LastSelection = content.Selection
            game.LastCategory = content.Category
            game.UsedItems[content.Selection] = true
            
            var activePlayers int
            var winner string
            
            // Add letter to challenger
            for _, p := range game.Players {
                if p.Name == game.ChallengeState.ChallengerName {
                    p.Letters++
                    // Start a new round since a player got a letter
                    if len(game.SelectionHistory) > 0 {
                        game.SelectionRounds = append(game.SelectionRounds, game.SelectionHistory)
                    }
                    game.SelectionHistory = make([]string, 0)
                    game.IsNewRound = true
                    if p.Letters >= 4 {
                        // Mark challenger as eliminated
                        p.IsEliminated = true
                        // Send elimination notification
                        if p.Conn != nil {
                            p.Conn.WriteJSON(map[string]string{
                                "type": "player_eliminated",
                                "message": "You've been eliminated, better luck next time!",
                            })
                        }
                    }
                    break
                }
            }

            // Count active players and find potential winner
            for _, p := range game.Players {
                if !p.IsEliminated {
                    activePlayers++
                    winner = p.Name
                }
            }

            if activePlayers == 1 {
                game.Mu.Unlock()
                handleGameEnded(game, fmt.Sprintf("%s wins!", winner))
                return
            }

            
            // Reset game state for next round
            game.ChallengeState = nil
            game.LastSelection = ""
            game.LastCategory = ""
            game.RoundStarted = false
            game.TimeLeft = 30

            // Find next active player
            nextPlayer := (game.CurrentPlayer + 1) % len(game.Players)
            for game.Players[nextPlayer].IsEliminated {
                nextPlayer = (nextPlayer + 1) % len(game.Players)
            }
            game.CurrentPlayer = nextPlayer
            
            game.Mu.Unlock()
            broadcastGameState(game)
            startTimer(game)
            return
        }
        
        // Normal turn handling
        if game.Players[game.CurrentPlayer].Name == player.Name && game.ChallengeState == nil {
            game.LastSelection = content.Selection
            game.LastCategory = content.Category
            game.UsedItems[content.Selection] = true
            game.SelectionHistory = append(game.SelectionHistory, content.Selection)

            // Find next active player
            nextPlayer := (game.CurrentPlayer + 1) % len(game.Players)
            for game.Players[nextPlayer].IsEliminated {
                nextPlayer = (nextPlayer + 1) % len(game.Players)
            }
            game.CurrentPlayer = nextPlayer
            
            game.RoundStarted = true
            game.TimeLeft = 30
            
            game.Mu.Unlock()
            broadcastGameState(game)
            startTimer(game)
        } else {
            game.Mu.Unlock()
        }

    
    case "reject_player":
        var content struct {
            PlayerName string `json:"playerName"`
        }
        if err := json.Unmarshal(msg.Content, &content); err != nil {
            log.Printf("Error unmarshaling reject player: %v", err)
            return
        }
        
        game.Mu.Lock()
        if player.Name == game.CreatorID {
            if _, exists := game.JoinRequests[content.PlayerName]; exists {
                // Get the pending connection before cleanup
                if pendingConn := game.PendingConns[content.PlayerName]; pendingConn != nil {
                    // Notify the rejected player
                    pendingConn.WriteJSON(map[string]string{
                        "type": "error",
                        "message": "Your join request was rejected",
                    })
                }
                
                // Clean up after notification
                delete(game.PendingConns, content.PlayerName)
                delete(game.JoinRequests, content.PlayerName)
                
                game.Mu.Unlock()
                broadcastGameState(game)
                return
            }
        }
        game.Mu.Unlock()

    case "remove_player":
        var content struct {
            PlayerName string `json:"name"`
        }
        if err := json.Unmarshal(msg.Content, &content); err != nil {
            log.Printf("Error unmarshaling remove player: %v", err)
            return
        }
        
        game.Mu.Lock()
        
        // if the game has started, mark the player as eliminated
        if game.InProgress {
            for i, p := range game.Players {
                if p.Name == content.PlayerName {
                    game.Players[i].IsEliminated = true
                    break
                }
            }
        }
        // otherwise, remove the player from the join requests
        if game.JoinRequests != nil {
            delete(game.JoinRequests, content.PlayerName)
        }

        game.Mu.Unlock()
        broadcastGameState(game)

    case "start_game":
        game.Mu.Lock()
        if player.Name == game.CreatorID && !game.InProgress && len(game.Players) >= 2 {
            game.InProgress = true
            game.CurrentPlayer = 0  // Start with the first player
            game.Mu.Unlock()
            broadcastGameState(game)
            broadcastAvailableGames() // Add broadcast to update lobby
            startTimer(game) // Start timer for first player
        } else {
            game.Mu.Unlock()
        }

    case "start_turn":
        game.Mu.Lock()
        if game.Players[game.CurrentPlayer].Name == player.Name {
            game.Mu.Unlock()
            startTimer(game)
        } else {
            game.Mu.Unlock()
        }
    
    case "time_expired":
        game.Mu.Lock()
        // Only handle time expiry if it's from the current player
        if game.Players[game.CurrentPlayer].Name == player.Name {
            // Call handleTimeExpiry directly to ensure consistent behavior
            game.Mu.Unlock()
            handleTimeExpiry(game)
            return
        }
        game.Mu.Unlock()
    }

}

func handleGetGames(w http.ResponseWriter, r *http.Request) {
	gamesMutex.Lock()
	defer gamesMutex.Unlock()

	type GameInfo struct {
		ID        string `json:"id"`
		CreatorID string `json:"creatorId"`
	}
	var gameList []GameInfo
	for gameID, game := range games {
		// Only include games that are not in progress
		if (!game.InProgress) {
			gameList = append(gameList, GameInfo{
				ID:        gameID,
				CreatorID: game.CreatorID,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(gameList)
}

func handleJoinGame(w http.ResponseWriter, r *http.Request) {
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

    if req.GameID == "" || req.Name == "" {
        log.Println("Missing required fields")
        http.Error(w, "GameID and Name are required", http.StatusBadRequest)
        return
    }

    gamesMutex.Lock()
    game, exists := games[req.GameID]
    if !exists {
        gamesMutex.Unlock()
        log.Printf("Game not found: %s", req.GameID)
        http.Error(w, "Game not found", http.StatusNotFound)
        return
    }
    gamesMutex.Unlock()

    game.Mu.Lock()
    // Check if game is already in progress
    if game.InProgress {
        game.Mu.Unlock()
        http.Error(w, "Game already in progress", http.StatusBadRequest)
        return
    }

    // Check if player name is already taken
    for _, player := range game.Players {
        if player.Name == req.Name {
            game.Mu.Unlock()
            http.Error(w, "Player name already taken", http.StatusConflict)
            return
        }
    }

    // Add join request if not the creator
    if game.CreatorID != req.Name {
        game.JoinRequests[req.Name] = JoinRequest{Name: req.Name, Message: req.Message}
    } else {
        // If it's the creator, add them directly to players
        game.Players = append(game.Players, &Player{
            Name: req.Name,
            IsActive: true,
        })
    }
    game.Mu.Unlock()

    // Send success response
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(map[string]string{"status": "success"})

    // Broadcast updates after releasing locks
    broadcastGameState(game)
    broadcastAvailableGames()
}

func handlePlayerDisconnect(game *Game, player *Player) {
    game.Mu.Lock()
    defer game.Mu.Unlock()

    // Mark player as inactive instead of removing them
    for _, p := range game.Players {
        if p == player {
            p.IsActive = false
            p.Conn = nil
            break
        }
    }

    // If creator disconnects or game is in progress, end the game
    if player.Name == game.CreatorID || game.InProgress {
        gamesMutex.Lock()
        delete(games, game.ID)
        gamesMutex.Unlock()
        
        // Broadcast updated game list to lobby
        broadcastAvailableGames()
        
        // Notify remaining players
        message := "Game creator disconnected"
        if game.InProgress && player.Name != game.CreatorID {
            message = fmt.Sprintf("Player %s disconnected. Game ended.", player.Name)
        }
        
        for _, p := range game.Players {
            if p.Conn != nil {
                p.Conn.WriteJSON(map[string]string{
                    "type": "game_ended",
                    "message": message,
                })
            }
        }
        return
    }

    broadcastGameState(game)
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
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

    encodedQuery := url.QueryEscape(query)
    searchUrl := fmt.Sprintf("https://en.wikipedia.org/w/api.php?action=query&format=json&list=search&origin=*&srsearch=%s", encodedQuery)
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

    // The response is a JSON object with a "query" field which is a dict
    // with a "search" props which includes a list of dicts, each of which
    // has a "title" field. The "search" field may also be null.
    var data map[string]interface{}
    if err := json.Unmarshal(body, &data); err != nil {
        log.Printf("Failed to unmarshal response body: %v\n", err)
        http.Error(w, "Failed to unmarshal response body", http.StatusInternalServerError)
        return
    }

    if data["query"].(map[string]interface{})["search"] == nil {
        // return an empty list
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode([]string{})
        return
    }

    // Get the titles of the search results
    var titles []string
    for _, result := range data["query"].(map[string]interface{})["search"].([]interface{}) {
        title := result.(map[string]interface{})["title"].(string)
        titles = append(titles, title)
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
    // for actors and "cinematography" for movies
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

}

func handleTimeExpiry(game *Game) {
    game.Mu.Lock()

    // Validate current state
    if len(game.Players) <= 1 || game.CurrentPlayer >= len(game.Players) {
        game.Mu.Unlock()
        return
    }

    var playerToEliminate *Player
    var nextPlayer int
    var isGameOver bool
    var winner string
    var activePlayers int

    // Add letter to current player and handle potential elimination
    for i, p := range game.Players {
        if i == game.CurrentPlayer {
            if !p.IsEliminated {
                p.Letters++
                // Start a new round since a player got a letter
                if len(game.SelectionHistory) > 0 {
                    game.SelectionRounds = append(game.SelectionRounds, game.SelectionHistory)
                }
                game.SelectionHistory = make([]string, 0)
                game.IsNewRound = true

                // If player has spelled BOMB, mark them for elimination
                if p.Letters >= 4 {
                    playerToEliminate = p
                    p.IsEliminated = true
                }
            }
            break
        }
    }

    // Count active players and find winner if needed
    for _, p := range game.Players {
        if !p.IsEliminated {
            activePlayers++
            winner = p.Name
        }
    }

    // Handle elimination if needed
    if playerToEliminate != nil {
        // Send elimination message
        if playerToEliminate.Conn != nil {
            playerToEliminate.Conn.WriteJSON(map[string]string{
                "type": "player_eliminated",
                "message": "You've been eliminated, better luck next time!",
            })
        }

        // Check if game is over (only one active player remaining)
        if activePlayers == 1 {
            isGameOver = true
        }
    }

    // Find next active player
    if !isGameOver {
        nextPlayer = (game.CurrentPlayer + 1) % len(game.Players)
        // Keep moving to next player until we find an active one
        for game.Players[nextPlayer].IsEliminated {
            nextPlayer = (nextPlayer + 1) % len(game.Players)
        }

        // Update game state for next turn
        game.LastSelection = ""
        game.LastCategory = ""
        game.RoundStarted = false
        if game.Timer != nil {
            game.Timer.Stop()
        }
        game.Timer = nil
        game.TimeLeft = 30
        game.ChallengeState = nil
        game.CurrentPlayer = nextPlayer
    }

    game.Mu.Unlock()

    // Always broadcast state first so UI can update
    broadcastGameState(game)

    // Handle game end if needed
    if isGameOver {
        handleGameEnded(game, fmt.Sprintf("%s wins!", winner))
    }
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
    if (err != nil) {
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

    if (!exists) {
        conn.WriteJSON(map[string]string{
            "type": "error",
            "message": "Game not found",
        })
        conn.Close()
        return
    }

    game.Mu.Lock()
    
    var player *Player
    if msg.IsReconnect {
        // Check if player was previously admitted
        for _, p := range game.Players {
            if p.Name == msg.Name {
                p.Conn = conn
                player = p
                break
            }
        }
    } else if msg.Name == game.CreatorID {
        // First player joining must be the creator
        player = &Player{
            Name: msg.Name,
            Conn: conn,
            IsActive: true,
        }
        game.Players = append(game.Players, player)
    } else {
        // For non-creator players, store their connection in PendingConns if they have a join request
        if _, hasJoinRequest := game.JoinRequests[msg.Name]; hasJoinRequest {
            game.PendingConns[msg.Name] = conn
            player = &Player{
                Name: msg.Name,
                Conn: conn,
                IsActive: false,
            }
        }
    }

    if player == nil {
        game.Mu.Unlock()
        conn.WriteJSON(map[string]string{
            "type": "error",
            "message": "Not authorized to join this game",
        })
        conn.Close()
        return
    }

    game.Mu.Unlock()

    // Broadcast state after releasing the mutex
    broadcastGameState(game)

    // Start handling the connection without holding any locks
    handleConnection(conn, game, player)
}

func main() {
    http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/create-game", handleCreateGame)
	http.HandleFunc("/join-game", handleJoinGame)
	http.HandleFunc("/get-games", handleGetGames)
	http.HandleFunc("/search", handleSearch)

	fmt.Println("Server running on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, "index.html")
}

func startTimer(game *Game) {
    game.Mu.Lock()
    if game.Timer != nil {
        game.Timer.Stop()
    }

    // Reset timer state
    game.TimeLeft = 30
    game.Timer = time.NewTimer(30 * time.Second)
    
    // Broadcast initial state with new timer
    game.Mu.Unlock()
    broadcastGameState(game)
    
    go func() {
        <-game.Timer.C
        // When timer expires, update game state and broadcast immediately
        game.Mu.Lock()
        game.TimeLeft = 0  // Ensure timeLeft is set to 0
        game.Timer = nil
        game.Mu.Unlock()
        
        // Broadcast that time is up before processing expiry
        broadcastGameState(game)
        
        // Then handle the expiry
        handleTimeExpiry(game)
    }()
}
