// On-disk session registry: sessions live under ~/.rysh/sessions/<id>/
// (the messages.log stream plus an optional name file), and two global
// append-only ledgers record their lifecycle — ~/.rysh/created ("<ts> <num>
// <id>", one line per session) and ~/.rysh/updated ("<ts> <id>", one line
// per user activity). The ledgers are the source of truth for listing and
// ordering; there is no global "current session" pointer, because many rysh
// processes can run at once and each keeps its own active session.
package session

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Meta is one session's identity as recorded on disk.
type Meta struct {
	Num     int    // session number (display ordering)
	ID      string // e.g. s1a2b3c
	Name    string // display name; "" renders as 新会话
	Created int64  // creation time, unix milliseconds
}

// Store is the on-disk session registry rooted at a .rysh directory.
type Store struct {
	root string
}

// NewStore returns a Store rooted at $HOME/.rysh. It errors when the home
// directory cannot be determined (no persistence possible).
func NewStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("session store: home dir: %w", err)
	}
	return &Store{root: filepath.Join(home, ".rysh")}, nil
}

// NewStoreAt returns a Store rooted at an explicit directory (used by tests).
func NewStoreAt(root string) *Store {
	return &Store{root: root}
}

// Root returns the store's root directory (~/.rysh by default).
func (s *Store) Root() string { return s.root }

// SessionsDir returns the per-session directory (~/.rysh/sessions).
func (s *Store) SessionsDir() string { return filepath.Join(s.root, "sessions") }

// CtlDir returns the per-instance control/attachment directory
// (~/.rysh/ctl), holding <pid>.cmd control channels and <pid>.sess
// attachment records.
func (s *Store) CtlDir() string { return filepath.Join(s.root, "ctl") }

// SessionDir returns the directory for one session.
func (s *Store) SessionDir(id string) string {
	return filepath.Join(s.SessionsDir(), id)
}

// NewID returns a random session id like s1a2b3c.
func NewID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("s%x", b)
}

// CreateSession creates a new session: a fresh id, the next number, the
// default 新会话 name, and a ledger line appended to created.
func (s *Store) CreateSession() (*Meta, error) {
	id := NewID()
	num, err := s.nextNum()
	if err != nil {
		return nil, err
	}
	m := &Meta{Num: num, ID: id, Created: time.Now().UnixMilli()}
	if err := s.appendCreated(m); err != nil {
		return nil, err
	}
	return m, nil
}

// EnsureSession returns the meta for id, creating a ledger entry for it when
// absent (used when RYSH_SESSION_ID overrides the startup session).
func (s *Store) EnsureSession(id string) (*Meta, error) {
	metas, err := s.ListSessions()
	if err != nil {
		return nil, err
	}
	for i := range metas {
		if metas[i].ID == id {
			return &metas[i], nil
		}
	}
	num, err := s.nextNum()
	if err != nil {
		return nil, err
	}
	m := &Meta{Num: num, ID: id, Created: time.Now().UnixMilli()}
	if err := s.appendCreated(m); err != nil {
		return nil, err
	}
	return m, nil
}

// ListSessions returns all sessions ordered by number, each with its display
// name merged from the per-session name file. Unparseable created lines are
// skipped.
func (s *Store) ListSessions() ([]Meta, error) {
	data, err := os.ReadFile(s.createdPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var metas []Meta
	for _, line := range strings.Split(string(data), "\n") {
		m, ok := parseCreatedLine(line)
		if !ok {
			continue
		}
		m.Name = s.LoadName(m.ID)
		metas = append(metas, m)
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Num < metas[j].Num })
	return metas, nil
}

// TouchUpdated records a user activity on id by appending "<ts> <id>" to the
// updated ledger, unless the last record already names the same session
// (consecutive updates to one session are merged so the ledger keeps its
// ordering signal).
func (s *Store) TouchUpdated(id string) error {
	last, _ := s.lastUpdatedID()
	if last == id {
		return nil
	}
	line := fmt.Sprintf("%d %s\n", time.Now().UnixMilli(), id)
	return appendLine(s.updatedPath(), line)
}

// LastUpdatedID returns the id of the most recently updated session, or ""
// when there is no updated record.
func (s *Store) LastUpdatedID() string {
	id, _ := s.lastUpdatedID()
	return id
}

// UpdatedTimes returns each session's most recent updated-ledger timestamp
// (unix milliseconds). Sessions never recorded in the ledger are absent
// from the map; an unreadable ledger yields an empty map.
func (s *Store) UpdatedTimes() map[string]int64 {
	out := map[string]int64{}
	f, err := os.Open(s.updatedPath())
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		ts, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if ts > out[fields[1]] {
			out[fields[1]] = ts
		}
	}
	return out
}

// SaveName records a session's display name in its per-session name file, so
// the created ledger stays append-only.
func (s *Store) SaveName(id, name string) error {
	dir := s.SessionDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "name"), []byte(name), 0o644)
}

// LoadName returns a session's display name, or "" when unset.
func (s *Store) LoadName(id string) string {
	data, err := os.ReadFile(filepath.Join(s.SessionDir(id), "name"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (s *Store) createdPath() string { return filepath.Join(s.root, "created") }
func (s *Store) updatedPath() string { return filepath.Join(s.root, "updated") }

func (s *Store) nextNum() (int, error) {
	metas, err := s.ListSessions()
	if err != nil {
		return 1, err
	}
	max := 0
	for _, m := range metas {
		if m.Num > max {
			max = m.Num
		}
	}
	return max + 1, nil
}

func (s *Store) appendCreated(m *Meta) error {
	if err := os.MkdirAll(s.SessionsDir(), 0o755); err != nil {
		return err
	}
	return appendLine(s.createdPath(), fmt.Sprintf("%d %d %s\n", m.Created, m.Num, m.ID))
}

// lastUpdatedID reads the last non-empty updated line and returns the id it
// names.
func (s *Store) lastUpdatedID() (string, error) {
	f, err := os.Open(s.updatedPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	last := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if fields := strings.Fields(line); len(fields) == 2 {
			last = fields[1]
		}
	}
	return last, sc.Err()
}

// parseCreatedLine parses one "<ts> <num> <id>" line.
func parseCreatedLine(line string) (Meta, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Meta{}, false
	}
	parts := strings.Fields(line)
	if len(parts) != 3 {
		return Meta{}, false
	}
	ts, err1 := strconv.ParseInt(parts[0], 10, 64)
	num, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return Meta{}, false
	}
	return Meta{Num: num, ID: parts[2], Created: ts}, true
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
