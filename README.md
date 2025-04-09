## BOMB

### Description

This is a game for 2-4 players. There are no logins, so to start gameplay the user simply selects how many players will participate and chooses a name for each player.

### Gameplay

Each turn can take a maximum of 30 seconds, and a countdown is displayed on the screen.

The first player selects either a movie or an actor, using the search bar.

Once they've made a selection, play continues and player 2 must select. If the previous selection was a movie, they must name an actor who appeared in that movie. If the previous selection was an actor, they must name a movie in which that actor played. Gameplay continues this way.

If a player runs out of time or clicks the "Challenge" button, the previous player must make a selection. If their selection is valid (meaning movie and actor match), the challenging player receives a letter (the letters a B, O, M, and B, in that order). If the selection is invalid or they run out of time, the challenged player receives a letter.

Players are eliminated if they've spelled out BOMB. The final player who was not eliminated is the winner.

A list of actors and a list of movies is displayed on the screen. Actors and movies cannot be reused at any time.

### Technical Overview

The first iteration only allowed local play on a single device.

The new iteration is a web app which allows users to play online. There is no database, so the game state is kept in the server. Users can join a game, and the game state is synchronized across all users in the game using web sockets. No account creation is required, any user can create a game and select their name as they'd like it to appear. Games can be started once at least two players have joined, and the creator decides when to start the game. When players request to join a game, they choose their name and can optionally send a message to the creator. The creator decides whether to admit them or not. Games cannot be joined once in progress. If the creator leaves, the game ends. Games cannot be stopped and started, but we do keep track of the current game's ID using local storage in case a user refreshes the page.
