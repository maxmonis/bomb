## BOMB

### Description

This is an online game for two or more players. There are no logins, so to start gameplay a user simply needs to provide a name to create a game or join an existing one (game join requests are sent to the creator and must be approved). Games cannot be joined once in progress. If any player leaves during gameplay, the game ends.

### Gameplay

Each turn can take a maximum of 30 seconds, and a countdown is displayed on the screen.

The first player selects either a movie or an actor, using the search bar.

Once they've made a selection, it's player 2's turn. If the previous selection was a movie, they must name an actor who appeared in that movie. If the previous selection was an actor, they must name a movie in which that actor appeared. Gameplay continues this way.

If a player runs out of time, they receive a letter (the letters are B, O, M, and B, in that order). If a player clicks the "Challenge" button, the previous player must make a selection. If their selection is valid (meaning movie and actor match), the challenging player receives a letter. If the selection is invalid or they give up or run out of time, the challenged player receives a letter.

A new round begins whenever a player gets a letter. All rounds of gameplay are displayed on the screen. Actors and movies cannot be reused at any time.

Players are eliminated once they've spelled out BOMB. The final player who was not eliminated is the winner.

### Technical Overview

This is a simple Go server with a frontend written in HTML, CSS, and JavaScript. It uses web sockets to keep track of the game state. There's no database.

### Development

To start the dev server, run:

```bash
go run main.go
```

You'll need to manually stop and restart the server if you make changes.

### Deployment

To deploy on Fly.io using the Dockerfile, run:

```bash
flyctl deploy
```
