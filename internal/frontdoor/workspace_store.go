package frontdoor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const workspaceStoreVersion = 1
const workspaceStoreMaxBytes = 256 << 10
const workspaceStoreMaxRecords = 64
const workspacePaneCap = 6
const workspaceSplitMaxDepth = 3
const workspaceSplitMaxChildren = 4
const workspaceNameMaxRunes = 128

var errWorkspaceConflict = errors.New("workspace revision conflict")
var errWorkspaceNotFound = errors.New("workspace not found")
var errWorkspaceValidation = errors.New("invalid workspace")
var errWorkspaceStoreFull = errors.New("workspace store full")
var errWorkspaceStoreUnavailable = errors.New("workspace store unavailable")

type WorkspaceSession struct {
	Realm  string `json:"realm"`
	Server string `json:"server"`
	Name   string `json:"name"`
}

// WorkspaceNode is the closed v1 split grammar. Semantic validation enforces
// the kind-specific field set, so a leaf cannot smuggle split fields and a
// split cannot persist authority-shaped leaf data.
type WorkspaceNode struct {
	Kind      string            `json:"kind"`
	Session   *WorkspaceSession `json:"session,omitempty"`
	OnMissing string            `json:"on_missing,omitempty"`
	AliasHint string            `json:"alias_hint,omitempty"`
	Direction string            `json:"direction,omitempty"`
	Weights   []int             `json:"weights,omitempty"`
	Children  []WorkspaceNode   `json:"children,omitempty"`
}

type WorkspaceRecord struct {
	WorkspaceID    string        `json:"workspace_id"`
	Name           string        `json:"name"`
	NormalizedName string        `json:"normalized_name"`
	Revision       uint64        `json:"revision"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	Tree           WorkspaceNode `json:"tree"`
}

type workspaceStoreFile struct {
	Version    int               `json:"version"`
	Workspaces []WorkspaceRecord `json:"workspaces"`
}

type workspaceStore struct {
	mu      sync.Mutex
	file    *durableFile
	now     func() time.Time
	newID   func() (string, error)
	records map[string]WorkspaceRecord
	names   map[string]string
}

func newWorkspaceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func validWorkspaceID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func workspaceLabel(value string) (string, bool) {
	if value == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value || utf8.RuneCountInString(value) > workspaceNameMaxRunes {
		return "", false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return strings.ToLower(value), true
}

func validateWorkspaceTree(root WorkspaceNode) error {
	seen := map[string]bool{}
	leaves := 0
	var walk func(WorkspaceNode, int) error
	walk = func(node WorkspaceNode, depth int) error {
		switch node.Kind {
		case "leaf":
			if node.Session == nil || node.Direction != "" || node.Weights != nil || node.Children != nil {
				return errWorkspaceValidation
			}
			if node.OnMissing != "offer" && node.OnMissing != "create" && node.OnMissing != "skip" {
				return errWorkspaceValidation
			}
			for _, value := range []string{node.Session.Realm, node.Session.Server, node.Session.Name} {
				if _, ok := workspaceLabel(value); !ok {
					return errWorkspaceValidation
				}
			}
			if node.AliasHint != "" {
				if _, ok := workspaceLabel(node.AliasHint); !ok {
					return errWorkspaceValidation
				}
			}
			leaves++
			if leaves > workspacePaneCap {
				return errWorkspaceValidation
			}
			key := node.Session.Realm + "\x00" + node.Session.Server + "\x00" + node.Session.Name
			if seen[key] {
				return errWorkspaceValidation
			}
			seen[key] = true
			return nil
		case "split":
			if node.Session != nil || node.OnMissing != "" || node.AliasHint != "" || (node.Direction != "row" && node.Direction != "column") || depth > workspaceSplitMaxDepth {
				return errWorkspaceValidation
			}
			if len(node.Children) < 2 || len(node.Children) > workspaceSplitMaxChildren || len(node.Weights) != len(node.Children) {
				return errWorkspaceValidation
			}
			for _, weight := range node.Weights {
				if weight < 1 || weight > 100 {
					return errWorkspaceValidation
				}
			}
			for _, child := range node.Children {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
			return nil
		default:
			return errWorkspaceValidation
		}
	}
	if err := walk(root, 1); err != nil || leaves == 0 {
		return errWorkspaceValidation
	}
	return nil
}

func cloneWorkspaceNode(node WorkspaceNode) WorkspaceNode {
	copy := node
	if node.Session != nil {
		session := *node.Session
		copy.Session = &session
	}
	copy.Weights = append([]int(nil), node.Weights...)
	if node.Children != nil {
		copy.Children = make([]WorkspaceNode, len(node.Children))
		for i := range node.Children {
			copy.Children[i] = cloneWorkspaceNode(node.Children[i])
		}
	}
	return copy
}

func cloneWorkspaceRecord(record WorkspaceRecord) WorkspaceRecord {
	record.Tree = cloneWorkspaceNode(record.Tree)
	return record
}

func newWorkspaceStore(path string) (*workspaceStore, error) {
	file, err := openDurableFile(path, "workspace store", workspaceStoreMaxBytes, errWorkspaceStoreUnavailable)
	if err != nil {
		return nil, err
	}
	store := &workspaceStore{file: file, now: time.Now, newID: newWorkspaceID, records: map[string]WorkspaceRecord{}, names: map[string]string{}}
	if err := store.load(); err != nil {
		_ = file.fs.(rootAliasFS).root.Close()
		return nil, err
	}
	return store, nil
}

func (store *workspaceStore) load() error {
	data, exists, err := store.file.read()
	if err != nil {
		return err
	}
	if !exists {
		return store.persistLocked()
	}
	var file workspaceStoreFile
	if err := decodeStrict(data, &file); err != nil {
		return fmt.Errorf("invalid workspace store: %w", err)
	}
	if file.Version != workspaceStoreVersion || len(file.Workspaces) > workspaceStoreMaxRecords {
		return errWorkspaceStoreUnavailable
	}
	for _, record := range file.Workspaces {
		normalized, ok := workspaceLabel(record.Name)
		if !ok || record.NormalizedName != normalized || !validWorkspaceID(record.WorkspaceID) || record.Revision == 0 || record.CreatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) || validateWorkspaceTree(record.Tree) != nil {
			return errWorkspaceStoreUnavailable
		}
		if _, duplicate := store.records[record.WorkspaceID]; duplicate || store.names[normalized] != "" {
			return errWorkspaceStoreUnavailable
		}
		record = cloneWorkspaceRecord(record)
		store.records[record.WorkspaceID] = record
		store.names[normalized] = record.WorkspaceID
	}
	return nil
}

func (store *workspaceStore) persistLocked() error {
	list := make([]WorkspaceRecord, 0, len(store.records))
	for _, record := range store.records {
		list = append(list, cloneWorkspaceRecord(record))
	}
	sort.Slice(list, func(i, j int) bool { return list[i].WorkspaceID < list[j].WorkspaceID })
	data, err := json.Marshal(workspaceStoreFile{Version: workspaceStoreVersion, Workspaces: list})
	if err != nil {
		return err
	}
	if err := store.file.write(data); err != nil {
		if errors.Is(err, errStoreOversized) {
			return errWorkspaceStoreFull
		}
		return err
	}
	return nil
}

func (store *workspaceStore) availableLocked() error { return store.file.fault }

func (store *workspaceStore) list() ([]WorkspaceRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.availableLocked(); err != nil {
		return nil, err
	}
	list := make([]WorkspaceRecord, 0, len(store.records))
	for _, record := range store.records {
		list = append(list, cloneWorkspaceRecord(record))
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].NormalizedName == list[j].NormalizedName {
			return list[i].WorkspaceID < list[j].WorkspaceID
		}
		return list[i].NormalizedName < list[j].NormalizedName
	})
	return list, nil
}

func (store *workspaceStore) create(name string, tree WorkspaceNode) (WorkspaceRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.availableLocked(); err != nil {
		return WorkspaceRecord{}, err
	}
	normalized, ok := workspaceLabel(name)
	if !ok || validateWorkspaceTree(tree) != nil {
		return WorkspaceRecord{}, errWorkspaceValidation
	}
	if id := store.names[normalized]; id != "" {
		return cloneWorkspaceRecord(store.records[id]), errWorkspaceConflict
	}
	if len(store.records) >= workspaceStoreMaxRecords {
		return WorkspaceRecord{}, errWorkspaceStoreFull
	}
	var id string
	for tries := 0; tries < 16; tries++ {
		candidate, err := store.newID()
		if err != nil {
			return WorkspaceRecord{}, err
		}
		if validWorkspaceID(candidate) && store.records[candidate].WorkspaceID == "" {
			id = candidate
			break
		}
	}
	if id == "" {
		return WorkspaceRecord{}, errWorkspaceStoreUnavailable
	}
	now := store.now().UTC()
	record := WorkspaceRecord{WorkspaceID: id, Name: name, NormalizedName: normalized, Revision: 1, CreatedAt: now, UpdatedAt: now, Tree: cloneWorkspaceNode(tree)}
	store.records[id], store.names[normalized] = record, id
	if err := store.persistLocked(); err != nil {
		if store.file.fault == nil {
			delete(store.records, id)
			delete(store.names, normalized)
		}
		return WorkspaceRecord{}, err
	}
	return cloneWorkspaceRecord(record), nil
}

func (store *workspaceStore) update(id, name string, tree WorkspaceNode, revision uint64) (WorkspaceRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.availableLocked(); err != nil {
		return WorkspaceRecord{}, err
	}
	current, ok := store.records[id]
	if !ok {
		return WorkspaceRecord{}, errWorkspaceNotFound
	}
	if current.Revision != revision {
		return cloneWorkspaceRecord(current), errWorkspaceConflict
	}
	normalized, valid := workspaceLabel(name)
	if !valid || validateWorkspaceTree(tree) != nil {
		return WorkspaceRecord{}, errWorkspaceValidation
	}
	if owner := store.names[normalized]; owner != "" && owner != id {
		return cloneWorkspaceRecord(store.records[owner]), errWorkspaceConflict
	}
	next := current
	next.Name, next.NormalizedName, next.Revision, next.UpdatedAt, next.Tree = name, normalized, revision+1, store.now().UTC(), cloneWorkspaceNode(tree)
	delete(store.names, current.NormalizedName)
	store.records[id], store.names[normalized] = next, id
	if err := store.persistLocked(); err != nil {
		if store.file.fault == nil {
			store.records[id] = current
			if normalized != current.NormalizedName {
				delete(store.names, normalized)
			}
			store.names[current.NormalizedName] = id
		}
		return WorkspaceRecord{}, err
	}
	return cloneWorkspaceRecord(next), nil
}

func (store *workspaceStore) delete(id string, revision uint64) (WorkspaceRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.availableLocked(); err != nil {
		return WorkspaceRecord{}, err
	}
	current, ok := store.records[id]
	if !ok {
		return WorkspaceRecord{}, errWorkspaceNotFound
	}
	if current.Revision != revision {
		return cloneWorkspaceRecord(current), errWorkspaceConflict
	}
	delete(store.records, id)
	delete(store.names, current.NormalizedName)
	if err := store.persistLocked(); err != nil {
		if store.file.fault == nil {
			store.records[id], store.names[current.NormalizedName] = current, id
		}
		return WorkspaceRecord{}, err
	}
	return cloneWorkspaceRecord(current), nil
}
