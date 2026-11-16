package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kryptamine/herdr-auto-title/internal/herdr"
)

// layoutDocument is the core document the app keeps its spaces and tabs in:
// docs/architecture/desktop-api.md#a-core-with-no-window.
const layoutDocument = "herdr-app"

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

// documentWorld answers `ls` from the document: an unnamed tab reads as its
// position and the first tab as active, as the app would answer with no window.
func (c *Client) documentWorld(ctx context.Context, terminals []terminal) (world, error) {
	var got struct {
		Value document `json:"value"`
	}

	if err := rpc(ctx, c.core, "doc.get", docName{layoutDocument}, &got); err != nil {
		return world{}, err
	}

	byAddress := make(map[string]terminal, len(terminals))
	for _, t := range terminals {
		byAddress[t.Address] = t
	}

	var w world

	for _, s := range got.Value.Spaces {
		listed := space{ID: s.ID, Name: s.Name}

		for i, t := range s.Tabs {
			shown := tab{ID: t.ID, Name: t.Name, Active: i == 0}
			if shown.Name == "" {
				shown.Name = strconv.Itoa(i + 1)
			}

			shown.Terminals = entriesOf(t.Root, byAddress)
			listed.Tabs = append(listed.Tabs, shown)
		}

		w.Spaces = append(w.Spaces, listed)
	}

	return w, nil
}

// entriesOf is a tab's panes as `ls` would list them, in the tree's order.
func entriesOf(root layoutNode, byAddress map[string]terminal) []paneEntry {
	blocks := blocksOf(root)
	entries := make([]paneEntry, 0, len(blocks))

	for _, block := range blocks {
		entry := paneEntry{Terminal: block.Content.Terminal, Pane: block.ID}
		if known, ok := byAddress[entry.Terminal]; ok && known.Status != nil {
			entry.Status, entry.App = known.appStatus(), known.Status.App
		}

		entries = append(entries, entry)
	}

	return entries
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

// renameInDocument writes the tab's name into the document, read again when
// another writer, such as a connected app, got there first.
func (c *Client) renameInDocument(ctx context.Context, tabID, label string) error {
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
			return &herdr.APIError{
				Code:    herdr.CodeTabNotFound,
				Message: noTabPrefix + strconv.Quote(tabID),
			}
		}

		write := docWrite{Name: layoutDocument, Value: value, Version: got.Version}

		err = rpc(ctx, c.core, "doc.set", write, nil)
		if !errors.Is(err, errConflict) || attempt == renameAttempts {
			return err
		}
	}
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
