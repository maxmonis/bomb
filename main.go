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
	type GameInfo struct {
		ID        string `json:"id"`
		CreatorID string `json:"creatorId"`
	}
	var gameList []GameInfo
	for gameID, game := range games {
		gameList = append(gameList, GameInfo{
			ID:        gameID,
			CreatorID: game.CreatorID,
		})
	}
	gamesMutex.Unlock()

	message := struct {
		Type  string    `json:"type"`
		Games []GameInfo `json:"games"`
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

    // Clean up failed connections
    if len(failedConns) > 0 {
        game.Mu.Lock()
        for _, failedConn := range failedConns {
            // Remove from players if present
            for i, p := range game.Players {
                if p.Conn == failedConn {
                    game.Players = append(game.Players[:i], game.Players[i+1:]...)
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

    gameID := uuid.New().String()
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
    // Notify all players
    for _, p := range game.Players {
        p.Conn.WriteJSON(map[string]string{
            "type": "game_ended",
            "message": message,
        })
    }
    
    // Remove game from global state
    gamesMutex.Lock()
    delete(games, game.ID)
    gamesMutex.Unlock()
    
    // Broadcast updated game list to lobby
    broadcastAvailableGames()
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
            // Find the previous player
            previousPlayerIndex := (game.CurrentPlayer - 1 + len(game.Players)) % len(game.Players)
            previousPlayer := game.Players[previousPlayerIndex]
            
            // Set up challenge state
            game.ChallengeState = &ChallengeState{
                ChallengerName: player.Name,
                ChallengedName: previousPlayer.Name,
                ExpectedCategory: game.LastCategory, // The category they need to validate
            }
            
            // Give control back to the challenged player
            game.CurrentPlayer = previousPlayerIndex
            game.TimeLeft = 30 // Reset timer for challenged player
            
            game.Mu.Unlock()
            broadcastGameState(game)
            startTimer(game) // Start timer for challenged player
        } else {
            game.Mu.Unlock()
        }

    case "give_up":
        game.Mu.Lock()
        if game.ChallengeState != nil && game.ChallengeState.ChallengedName == player.Name {
            // Add letter to challenged player
            for i, p := range game.Players {
                if p.Name == player.Name {
                    p.Letters++
                    if p.Letters >= 4 {
                        // Remove player if they spell BOMB
                        game.Players = append(game.Players[:i], game.Players[i+1:]...)
                        
                        // If only one player remains, they win
                        if len(game.Players) == 1 {
                            game.Mu.Unlock()
                            handleGameEnded(game, fmt.Sprintf("%s wins!", game.Players[0].Name))
                            return
                        }
                    }
                    break
                }
            }
            
            // Reset game state for next round
            game.ChallengeState = nil
            game.LastSelection = ""
            game.LastCategory = ""
            game.RoundStarted = false
            game.TimeLeft = 30
            // Move to next player after the challenged player
            game.CurrentPlayer = (game.CurrentPlayer + 1) % len(game.Players)
            
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
            isValid := validateSelection(game, content.Selection, content.Category)
            
            if isValid {
                // Challenge failed, add letter to challenger and add valid selection to history
                game.SelectionHistory = append(game.SelectionHistory, content.Selection)
                game.LastSelection = content.Selection
                game.LastCategory = content.Category
                game.UsedItems[content.Selection] = true
                
                for i, p := range game.Players {
                    if p.Name == game.ChallengeState.ChallengerName {
                        p.Letters++
                        // Start a new round since a player got a letter
                        if len(game.SelectionHistory) > 0 {
                            game.SelectionRounds = append(game.SelectionRounds, game.SelectionHistory)
                            game.SelectionHistory = make([]string, 0)
                        }
                        game.IsNewRound = true
                        if p.Letters >= 4 {
                            // Remove challenger if they spell BOMB
                            game.Players = append(game.Players[:i], game.Players[i+1:]...)
                            
                            // If only one player remains, they win
                            if len(game.Players) == 1 {
                                game.Mu.Unlock()
                                handleGameEnded(game, fmt.Sprintf("%s wins!", game.Players[0].Name))
                                return
                            }
                        }
                        break
                    }
                }
            } else {
                // Challenge succeeded, add letter to challenged player
                for i, p := range game.Players {
                    if p.Name == player.Name {
                        p.Letters++
                        // Start a new round since a player got a letter
                        if len(game.SelectionHistory) > 0 {
                            game.SelectionRounds = append(game.SelectionRounds, game.SelectionHistory)
                            game.SelectionHistory = make([]string, 0)
                        }
                        game.IsNewRound = true
                        if p.Letters >= 4 {
                            // Remove challenged player if they spell BOMB
                            game.Players = append(game.Players[:i], game.Players[i+1:]...)
                            
                            // If only one player remains, they win
                            if len(game.Players) == 1 {
                                game.Mu.Unlock()
                                handleGameEnded(game, fmt.Sprintf("%s wins!", game.Players[0].Name))
                                return
                            }
                        }
                        break
                    }
                }
            }
            
            // Reset game state for next round
            game.ChallengeState = nil
            game.LastSelection = ""
            game.LastCategory = ""
            game.RoundStarted = false
            game.TimeLeft = 30
            // Move to next player after the challenged player
            game.CurrentPlayer = (game.CurrentPlayer + 1) % len(game.Players)
            
            game.Mu.Unlock()
            broadcastGameState(game)
            startTimer(game)
            return
        }
        
        // Normal turn handling
        if game.Players[game.CurrentPlayer].Name == player.Name && game.ChallengeState == nil {
            validSelection := !game.RoundStarted || validateSelection(game, content.Selection, content.Category)
            if validSelection {
                game.LastSelection = content.Selection
                game.LastCategory = content.Category
                game.UsedItems[content.Selection] = true
                game.SelectionHistory = append(game.SelectionHistory, content.Selection)
                game.CurrentPlayer = (game.CurrentPlayer + 1) % len(game.Players)
                game.RoundStarted = true
                game.TimeLeft = 30
                
                game.Mu.Unlock()
                broadcastGameState(game)
                startTimer(game)
            } else {
                game.Mu.Unlock()
                player.Conn.WriteJSON(map[string]string{
                    "type": "error",
                    "message": "Invalid selection",
                })
            }
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

    case "start_game":
        game.Mu.Lock()
        if player.Name == game.CreatorID && !game.InProgress && len(game.Players) >= 2 {
            game.InProgress = true
            game.CurrentPlayer = 0  // Start with the first player
            game.Mu.Unlock()
            broadcastGameState(game)
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

    // Check for game end
    game.Mu.Lock()
    if len(game.Players) == 1 {
        // Get winner under lock
        winner := game.Players[0].Name
        game.Mu.Unlock()
        
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
		gameList = append(gameList, GameInfo{
			ID:        gameID,
			CreatorID: game.CreatorID,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(gameList)
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

	// Remove player from the game
	for i, p := range game.Players {
		if p == player {
			game.Players = append(game.Players[:i], game.Players[i+1:]...)
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
			p.Conn.WriteJSON(map[string]string{
				"type": "game_ended",
				"message": message,
			})
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

    // If validating a selection, check the relationship
    lastSelection := r.URL.Query().Get("lastSelection")
    if lastSelection != "" {
        isValid := false
        if category == "actor" {
            isValid = validateMovieActorRelation(lastSelection, query) || true // TODO: Implement actual validation
        } else {
            isValid = validateMovieActorRelation(query, lastSelection) || true // TODO: Implement actual validation
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

}

func handleTimeExpiry(game *Game) {
    game.Mu.Lock()

    // Add letter to current player and handle potential elimination
    for i, p := range game.Players {
        if i == game.CurrentPlayer {
            p.Letters++
            // Start a new round since a player got a letter
            if len(game.SelectionHistory) > 0 {
                game.SelectionRounds = append(game.SelectionRounds, game.SelectionHistory)
                game.SelectionHistory = make([]string, 0)
            }
            game.IsNewRound = true
            // If player has spelled BOMB, remove them
            if p.Letters >= 4 {
                game.Players = append(game.Players[:i], game.Players[i+1:]...)
                i-- // Adjust index after removal
            }
            break
        }
    }
    
    // Check if game is over
    if len(game.Players) == 1 {
        handleGameEnded(game, "Game over! Winner: "+game.Players[0].Name)
        return
    }

    // Reset game state for next turn
    game.LastSelection = ""
    game.LastCategory = ""
    game.RoundStarted = false
    game.Timer = nil
    game.TimeLeft = 30  // Reset timer for next player
    game.ChallengeState = nil

    // Move to next player, handling wrap-around
    game.CurrentPlayer = (game.CurrentPlayer + 1) % len(game.Players)

    // Broadcast updated state
    game.Mu.Unlock()
    broadcastGameState(game)
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

    log.Printf("Received WebSocket message: %+v", msg)

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

    log.Printf("Attempting to acquire game mutex in handleWebSocket for game %s", msg.GameID)
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
    log.Printf("Released game mutex in handleWebSocket")

    // Broadcast state after releasing the mutex
    broadcastGameState(game)

    // Start handling the connection without holding any locks
    handleConnection(conn, game, player)
}

func main() {
	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/create-game", handleCreateGame)
	http.HandleFunc("/join-game", handleJoinGame)
	http.HandleFunc("/get-games", handleGetGames)
	http.HandleFunc("/search", handleSearch)

	fmt.Println("Server running on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
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

func validateMovieActorRelation(movieTitle, actorName string) bool {
    // Check movie page for actor
    if foundInMovie := checkMoviePage(movieTitle, actorName); foundInMovie {
        return true
    }
    
    // Check actor page for movie
    return checkActorPage(actorName, movieTitle)
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
