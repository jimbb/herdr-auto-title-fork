package desktop

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kryptamine/herdr-auto-title/internal/herdr"
)

// layoutDocument is the core document the app keeps its spaces and tabs in:
// docs/architecture/desktop-api.md#a-core-with-no-window.
const layoutDocument = "herdr-app"

const (
	coreSpaceLabel = "herdr.space"
	coreTabLabel   = "herdr.tab"
	ovisTabName    = "ovis.tab"
)

// A rename that keeps losing the document to other writers gives up after this.
const renameAttempts = 3

// errConflict is the core refusing a document write made against an older version.
var errConflict = errors.New("the layout document changed")

// document is what this reads of the app's layout. Renames rewrite the whole
// value, so they go through renameIn on the raw JSON instead.
type document struct {
	Spaces []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Tabs []struct {
			ID   string     `json:"id"`
			Name string     `json:"name"`
			Root layoutNode `json:"root"`
		} `json:"tabs"`
	} `json:"spaces"`
}

// layoutNode is a split or a block; a block holds one terminal.
type layoutNode struct {
	ID      string `json:"id"`
	Content *struct {
		Terminal string `json:"terminal"`
	} `json:"content"`
	Children []layoutNode `json:"children"`
}

// docName and docWrite are doc.get's and doc.set's parameters.
type docName struct {
	Name string `json:"name"`
}

type docWrite struct {
	Name    string `json:"name"`
	Value   any    `json:"value"`
	Version uint64 `json:"version"`
}

// coreStatus is a terminal's own report, as hc 0.1.18 and later carry it.
type coreStatus struct {
	App   string `json:"app"`
	State string `json:"state"`
}

func (c *Client) documentWorld(ctx context.Context, terminals []terminal) (world, error) {
	var got struct {
		Value document `json:"value"`
	}

	if err := rpc(ctx, c.core, "doc.get", docName{layoutDocument}, &got); err != nil {
		return world{}, err
	}

	return got.Value.world(terminals), nil
}

func (d document) world(terminals []terminal) world {
	byAddress := make(map[string]terminal, len(terminals))
	for _, t := range terminals {
		byAddress[t.Address] = t
	}

	placed := make(map[string]bool)

	var w world

	for _, s := range d.Spaces {
		listed := space{ID: s.ID, Name: s.Name}

		for i, t := range s.Tabs {
			shown := tab{
				ID: t.ID, Name: cmp.Or(t.Name, strconv.Itoa(i+1)), Active: i == 0,
				Terminals: entriesOf(t.Root, byAddress),
			}
			for _, pane := range shown.Terminals {
				placed[pane.Terminal] = true
			}

			listed.Tabs = append(listed.Tabs, shown)
		}

		w.Spaces = append(w.Spaces, listed)
	}

	addUnplaced(&w, terminals, placed)

	return w
}

func addUnplaced(w *world, terminals []terminal, placed map[string]bool) {
	spaces := make(map[string]int, len(w.Spaces))
	used := map[string]bool{"hc-other": true}

	for i, s := range w.Spaces {
		spaces[s.ID] = i
		used[s.ID] = true

		for _, t := range s.Tabs {
			used[t.ID] = true
		}
	}

	for _, t := range terminals {
		id := t.Labels[coreSpaceLabel]
		if id == "" || t.State != "live" || t.Address == "" ||
			strings.Contains(t.Address, "/") || placed[t.Address] {
			continue
		}

		i, exists := spaces[id]
		if !exists {
			if used[id] {
				continue
			}

			i = len(w.Spaces)
			spaces[id], used[id] = i, true
			w.Spaces = append(w.Spaces, space{ID: id})
		}

		addLabelPane(&w.Spaces[i], t, used)
	}
}

func addLabelPane(s *space, t terminal, used map[string]bool) {
	workspace, name := whereNames(t)
	if s.Name == "" {
		dir := cmp.Or(t.Foreground.CWD, t.CWD)
		s.Name = cmp.Or(
			strings.TrimSpace(workspace),
			t.Labels["ovis.workspace"],
			filepath.Base(dir),
		)
	}

	id := t.Labels[coreTabLabel]
	if id == "" || used[id] && s.tabIndex(id) < 0 {
		// Ovis gives terminals without a usable tab id an address-based tab.
		base := s.ID + ":" + t.Address
		id = base

		for n := 2; used[id] && s.tabIndex(id) < 0; n++ {
			id = base + ":" + strconv.Itoa(n)
		}
	}

	i := s.tabIndex(id)
	if i < 0 {
		i = len(s.Tabs)
		name = cmp.Or(name, t.Labels[ovisTabName], strconv.Itoa(i+1))
		s.Tabs = append(s.Tabs, tab{ID: id, Name: name, Active: i == 0})
		used[id] = true
	}

	s.Tabs[i].Terminals = append(s.Tabs[i].Terminals, corePane(t.Address, t.Address, t))
}

func (s space) tabIndex(id string) int {
	return slices.IndexFunc(s.Tabs, func(t tab) bool { return t.ID == id })
}

func whereNames(t terminal) (workspace, name string) {
	workspace, rest, _ := strings.Cut(t.Labels["where"], " / ")
	name, _, _ = strings.Cut(rest, " / ")

	return workspace, strings.TrimSpace(name)
}

// entriesOf is a tab's panes as `ls` would list them, in the tree's order.
func entriesOf(root layoutNode, byAddress map[string]terminal) []paneEntry {
	blocks := blocksOf(root)
	entries := make([]paneEntry, 0, len(blocks))

	for _, block := range blocks {
		address := block.Content.Terminal
		entries = append(entries, corePane(address, block.ID, byAddress[address]))
	}

	return entries
}

func corePane(address, pane string, t terminal) paneEntry {
	entry := paneEntry{Terminal: address, Pane: pane}
	if t.Status != nil {
		entry.Status, entry.App = t.appStatus(), t.Status.App
	}

	return entry
}

func blocksOf(node layoutNode) []layoutNode {
	if node.Content != nil && node.Content.Terminal != "" {
		return []layoutNode{node}
	}

	var blocks []layoutNode
	for _, child := range node.Children {
		blocks = append(blocks, blocksOf(child)...)
	}

	return blocks
}

// appStatus is the app's word for a terminal at rest: done while its last turn
// is past the one someone looked at (`herdr.seen`), idle once they have.
func (t terminal) appStatus() string {
	state := t.Status.State
	if state != herdr.AgentStatusIdle && state != herdr.AgentStatusDone {
		return state
	}

	if seen, err := strconv.ParseUint(t.Labels["herdr.seen"], 10, 64); err == nil && t.Turn > seen {
		return herdr.AgentStatusDone
	}

	return herdr.AgentStatusIdle
}

// renameCoreTab retries a document write if another writer got there first.
func (c *Client) renameCoreTab(ctx context.Context, tabID, label string) error {
	for attempt := 1; ; attempt++ {
		var got struct {
			Value   json.RawMessage `json:"value"`
			Version uint64          `json:"version"`
		}

		if err := rpc(ctx, c.core, "doc.get", docName{layoutDocument}, &got); err != nil {
			return err
		}

		value, found, err := renameIn(got.Value, tabID, strings.TrimSpace(label))
		if err != nil {
			return err
		}

		if !found {
			return c.renameUnplacedTab(ctx, got.Value, tabID, label)
		}

		write := docWrite{Name: layoutDocument, Value: value, Version: got.Version}

		err = rpc(ctx, c.core, "doc.set", write, nil)
		if !errors.Is(err, errConflict) || attempt == renameAttempts {
			return err
		}
	}
}

func (c *Client) renameUnplacedTab(
	ctx context.Context, raw json.RawMessage, tabID, label string,
) error {
	var d document
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("read %s: %w", layoutDocument, err)
	}

	terminals, err := c.terminals(ctx)
	if err != nil {
		return err
	}

	for _, s := range d.world(terminals).join(terminals) {
		if i := s.tabIndex(tabID); i >= 0 {
			return c.renameLabelPanes(ctx, s.Tabs[i].panes, label)
		}
	}

	return &herdr.APIError{Code: herdr.CodeTabNotFound, Message: noTabPrefix + strconv.Quote(tabID)}
}

func (c *Client) renameLabelPanes(ctx context.Context, panes []terminalPane, label string) error {
	// The first pane supplies the displayed name, so change it last.
	for i, p := range slices.Backward(panes) {
		labels := map[string]string{ovisTabName: label}
		if workspace, name := whereNames(p.terminal); name != "" {
			// Keep the workspace part so `where` no longer hides `ovis.tab`.
			labels["where"] = workspace
		}

		params := map[string]any{"terminal": p.terminal.Address, "labels": labels}
		if err := rpc(ctx, c.core, "terminal.label", params, nil); err != nil {
			if i < len(panes)-1 {
				return fmt.Errorf("%w: %w", herdr.ErrUnanswered, err)
			}

			return err
		}
	}

	return nil
}

// renameIn sets one tab's name in the document and keeps everything else in
// it, numbers included, exactly as the app wrote it.
func renameIn(raw json.RawMessage, tabID, name string) (any, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, false, fmt.Errorf("read %s: %w", layoutDocument, err)
	}

	spaces, _ := value["spaces"].([]any)
	for _, s := range spaces {
		listed, _ := s.(map[string]any)
		tabs, _ := listed["tabs"].([]any)

		for _, t := range tabs {
			if shown, _ := t.(map[string]any); shown != nil && shown["id"] == tabID {
				shown["name"] = name
				return value, true, nil
			}
		}
	}

	return value, false, nil
}

// rpc asks the core one JSON-RPC question on a connection of its own; a
// write against an older document version is errConflict.
func rpc(ctx context.Context, path, method string, params, result any) error {
	request := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}

	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
			Data    struct {
				Kind string `json:"kind"`
			} `json:"data"`
		} `json:"error"`
	}

	if err := ask(ctx, path, request, &reply); err != nil {
		return err
	}

	if reply.Error != nil {
		if reply.Error.Data.Kind == "conflict" {
			return fmt.Errorf("%w: %s", errConflict, reply.Error.Message)
		}

		return fmt.Errorf("%s: %s", method, reply.Error.Message)
	}

	if result == nil {
		return nil
	}

	if err := json.Unmarshal(reply.Result, result); err != nil {
		return fmt.Errorf("%w: decode %s: %w", herdr.ErrUnanswered, method, err)
	}

	return nil
}
