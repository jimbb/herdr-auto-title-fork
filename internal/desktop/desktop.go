// Package desktop answers the Herdr client interface from the desktop app and
// herdr-core, so the rest of Auto Title names their tabs as it names Herdr
// TUI's, and offers newer releases: docs/architecture/desktop-api.md.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kryptamine/herdr-auto-title/internal/herdr"
)

const (
	appHomeEnv = "HERDR_APP_HOME"
	appSocket  = "herdr-app.sock"

	// A tab.rename sends these spellings back: the app has one way to say a tab
	// is gone, and one to say the window took too long, whose request stays queued.
	noTabPrefix = "no tab "
	noAnswer    = "the window did not answer"
)

// Client speaks to the app's socket and the core's. Both answer one request
// per connection here, so like the TUI client it has nothing to reconnect.
type Client struct {
	app  string
	core string
	// coreLayout uses the core's layout document and terminal labels.
	coreLayout bool

	mu sync.Mutex
	// foregrounds is what each pane was running at the last snapshot.
	foregrounds map[string]herdr.PaneProcessInfoProcess
	// initialLabels holds the first label until it changes; an empty value
	// means a launcher's name must never be inferred again for that tab.
	initialLabels map[string]string
	// focused is the pane each tab last had focused.
	focused map[string]string

	sessions *sessionFinder
}

var _ herdr.Client = (*Client)(nil)

// New builds a client for this user's app and core. It performs no I/O: the
// window comes and goes, and a poll finding it closed simply fails.
func New(claudeDirs []string, coreLayout bool) (*Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate the home directory: %w", err)
	}

	return &Client{
		app:        appSocketPath(home),
		core:       coreSocketPath(home),
		coreLayout: coreLayout,
		sessions:   newSessionFinder(claudeDirs),
	}, nil
}

func appSocketPath(home string) string {
	if dir := os.Getenv(appHomeEnv); dir != "" {
		return filepath.Join(dir, appSocket)
	}

	return filepath.Join(home, ".config", "herdr-app", appSocket)
}

// Server is empty because a window restart is not a successor: nothing else
// starts an Auto Title for the desktop app, so this one must stay.
func (*Client) Server() string { return "" }

// Path is the socket that names this session's claim: the app's, or the core's
// when no app socket is read.
func (c *Client) Path() string {
	if c.coreLayout {
		return c.core
	}

	return c.app
}

// Snapshot joins the app's view to the core's. What each pane runs is kept for
// PaneProcesses: terminal.list already carries it, at no extra cost.
func (c *Client) Snapshot(ctx context.Context) (herdr.Snapshot, error) {
	world, focusedPane, terminals, err := c.read(ctx)
	if err != nil {
		return herdr.Snapshot{}, err
	}

	spaces := world.join(terminals)
	sessions := c.sessions.read(ctx, localPanesIn(spaces))

	c.mu.Lock()
	snapshot, foregrounds := c.build(spaces, focusedPane, sessions)
	c.foregrounds = foregrounds
	c.mu.Unlock()

	return snapshot, nil
}

// PaneProcesses reports the pane's foreground process, the only one the core
// names. A pane that has only just started reports none yet.
func (c *Client) PaneProcesses(
	_ context.Context,
	paneID string,
) ([]herdr.PaneProcessInfoProcess, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	process, known := c.foregrounds[paneID]
	if !known {
		return nil, &herdr.APIError{Code: herdr.CodePaneNotFound, Message: "no pane " + paneID}
	}

	if process.PID == 0 {
		return nil, nil
	}

	return []herdr.PaneProcessInfoProcess{process}, nil
}

func (c *Client) RenameTab(ctx context.Context, tabID, label string) error {
	if c.coreLayout {
		return c.renameCoreTab(ctx, tabID, label)
	}

	request := map[string]any{
		"do":   "tab.rename",
		"args": map[string]string{"id": tabID, "name": label},
	}

	var reply struct {
		Error string `json:"error"`
	}

	if err := ask(ctx, c.app, request, &reply); err != nil {
		return err
	}

	if strings.HasPrefix(reply.Error, noTabPrefix) {
		return &herdr.APIError{Code: herdr.CodeTabNotFound, Message: reply.Error}
	}

	return appError("tab.rename", reply.Error)
}

// read is the layout, the focused pane and the core's terminals. The document
// knows no focus: that lives in a window.
func (c *Client) read(ctx context.Context) (world, string, []terminal, error) {
	if c.coreLayout {
		terminals, err := c.terminals(ctx)
		if err != nil {
			return world{}, "", nil, err
		}

		world, err := c.documentWorld(ctx, terminals)

		return world, "", terminals, err
	}

	var world world
	if err := ask(ctx, c.app, map[string]bool{"ls": true}, &world); err != nil {
		return world, "", nil, err
	}

	if err := appError("ls", world.Error); err != nil {
		return world, "", nil, err
	}

	var status appStatus
	if err := ask(ctx, c.app, map[string]bool{"status": true}, &status); err != nil {
		return world, "", nil, err
	}

	if err := appError("status", status.Error); err != nil {
		return world, "", nil, err
	}

	terminals, err := c.terminals(ctx)

	return world, status.FocusedPane, terminals, err
}

// errRefused is the app's answer that it will not do what was asked, as
// opposed to no answer at all.
var errRefused = errors.New("the desktop app refused")

// appError is the app's error reply as an error: a slow window is unanswered,
// anything else a refusal. An `ls` or `status` that failed must fail the poll:
// read as an empty session, it drops every lock.
func appError(request, message string) error {
	switch message {
	case "":
		return nil
	case noAnswer:
		return fmt.Errorf("%w: %s: %s", herdr.ErrUnanswered, request, message)
	default:
		return fmt.Errorf("%w: %s: %s", errRefused, request, message)
	}
}

func (c *Client) terminals(ctx context.Context) ([]terminal, error) {
	var listed struct {
		Terminals []terminal `json:"terminals"`
	}

	if err := rpc(ctx, c.core, "terminal.list", map[string]any{}, &listed); err != nil {
		return nil, err
	}

	return listed.Terminals, nil
}
