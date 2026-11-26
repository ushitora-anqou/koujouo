/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	gorcon "github.com/gorcon/rcon"
)

// RCON parameters of the game process. RCON never leaves the pod: the
// game binds it to the loopback interface with a random password.
const (
	rconBindHost    = "127.0.0.1"
	rconDefaultPort = 55555
	rconDeadline    = 5 * time.Second
)

// playersOnlineCommand asks the game for the number of online players.
// The response looks like "Online players (2):", so the number is the
// first parenthesized integer.
const playersOnlineCommand = "/players online count"

// rePlayersCount matches the parenthesized count of the players online
// response.
var rePlayersCount = regexp.MustCompile(`\((\d+)\)`)

// rconConn is the part of the RCON client the agent uses. It is an
// interface so the graceful stop logic can be tested with a fake.
type rconConn interface {
	// PlayersOnline returns the number of players currently online.
	PlayersOnline(ctx context.Context) (int, error)

	// Chat sends a chat message to all players.
	Chat(ctx context.Context, message string) error

	// Close closes the connection.
	Close() error
}

// rconDialer connects to the RCON endpoint of the game. It is a
// function type so tests can substitute a fake implementation.
type rconDialer func(address, password string) (rconConn, error)

// dialRcon is the production dialer built on gorcon.
func dialRcon(address, password string) (rconConn, error) { //nolint:ireturn // Tests substitute the interface.
	conn, err := gorcon.Dial(address, password, gorcon.SetDeadline(rconDeadline))
	if err != nil {
		return nil, fmt.Errorf("could not connect to the RCON endpoint %s: %w", address, err)
	}

	return &gorconConn{conn: conn}, nil
}

// gorconConn adapts one authenticated gorcon connection to rconConn.
// A command whose context ends is aborted by closing the connection:
// the RCON protocol has no cancellation of its own.
type gorconConn struct {
	conn *gorcon.Conn
}

// PlayersOnline returns the number of players currently online.
func (c *gorconConn) PlayersOnline(ctx context.Context) (int, error) {
	response, err := c.execute(ctx, playersOnlineCommand)
	if err != nil {
		return 0, err
	}

	match := rePlayersCount.FindStringSubmatch(response)
	if match == nil {
		return 0, fmt.Errorf("could not parse the players online response %q: %w", response, errPlayersResponse)
	}

	players, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, fmt.Errorf("could not parse the players online response %q: %w", response, err)
	}

	return players, nil
}

// errPlayersResponse marks an unparseable players online response.
var errPlayersResponse = errors.New("unexpected players online response")

// Chat sends a chat message to all players. The message must not start
// with a slash, or the game would treat it as a command.
func (c *gorconConn) Chat(ctx context.Context, message string) error {
	_, err := c.execute(ctx, message)

	return err
}

// Close closes the underlying connection.
func (c *gorconConn) Close() error {
	err := c.conn.Close()
	if err != nil {
		return fmt.Errorf("could not close the RCON connection: %w", err)
	}

	return nil
}

// rconResult carries the outcome of one RCON command.
type rconResult struct {
	response string
	err      error
}

// execute runs one RCON command and aborts it by closing the connection
// when ctx is done first.
func (c *gorconConn) execute(ctx context.Context, command string) (string, error) {
	results := make(chan rconResult, 1)

	go func() {
		response, err := c.conn.Execute(command)
		results <- rconResult{response: response, err: err}
	}()

	select {
	case <-ctx.Done():
		_ = c.conn.Close()

		return "", fmt.Errorf("could not finish the RCON command: %w", ctx.Err())
	case result := <-results:
		if result.err != nil {
			return "", fmt.Errorf("could not execute the RCON command: %w", result.err)
		}

		return result.response, nil
	}
}

// rconAddress returns the RCON address of the game on the given port.
func rconAddress(port int) string {
	return net.JoinHostPort(rconBindHost, strconv.Itoa(port))
}
