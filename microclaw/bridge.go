package microclaw

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Chat represents a microclaw chat/channel.
type Chat struct {
	ChatID          int64
	ChatTitle       string
	ChatType        string
	LastMessageTime string
	Channel         string
	ExternalChatID  string
}

// Message represents a message from microclaw's database.
type Message struct {
	ID         string
	ChatID     int64
	SenderName string
	Content    string
	IsFromBot  int
	Timestamp  string
}

// MessageMeta contains metadata attached to messages sent from claude-squad.
type MessageMeta struct {
	RepoPath string `json:"repo_path,omitempty"`
	RepoID   string `json:"repo_id,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Program  string `json:"program,omitempty"`
}

// Bridge communicates with a running microclaw instance via its SQLite database.
type Bridge struct {
	MicroClawDir string
}

// NewBridge creates a new Bridge pointing at the given microclaw directory.
// If dir is empty, it defaults to ~/.microclaw.
func NewBridge(dir string) *Bridge {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".microclaw")
	}
	return &Bridge{MicroClawDir: dir}
}

func (b *Bridge) dbPath() string {
	return filepath.Join(b.MicroClawDir, "runtime", "microclaw.db")
}

// Available returns true if the microclaw DB exists.
func (b *Bridge) Available() bool {
	_, err := os.Stat(b.dbPath())
	return err == nil
}

func (b *Bridge) openDB() (*sql.DB, error) {
	dsn := b.dbPath() + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	return sql.Open("sqlite", dsn)
}

// ListChats returns all chats ordered by most recent activity.
func (b *Bridge) ListChats() ([]Chat, error) {
	db, err := b.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT chat_id, COALESCE(chat_title, ''), COALESCE(chat_type, 'private'),
		       COALESCE(last_message_time, ''), COALESCE(channel, ''), COALESCE(external_chat_id, '')
		FROM chats ORDER BY last_message_time DESC`)
	if err != nil {
		return nil, fmt.Errorf("failed to query chats: %w", err)
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ChatID, &c.ChatTitle, &c.ChatType, &c.LastMessageTime, &c.Channel, &c.ExternalChatID); err != nil {
			return nil, fmt.Errorf("failed to scan chat: %w", err)
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// GetRecentMessages returns recent messages across all chats, oldest first.
func (b *Bridge) GetRecentMessages(limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	db, err := b.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT COALESCE(id, ''), chat_id, COALESCE(sender_name, ''),
		       COALESCE(content, ''), COALESCE(is_from_bot, 0), COALESCE(timestamp, '')
		FROM messages ORDER BY timestamp DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query messages: %w", err)
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChatID, &m.SenderName, &m.Content, &m.IsFromBot, &m.Timestamp); err != nil {
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse so oldest is first
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// GetMessagesForChat returns recent messages for a specific chat, oldest first.
func (b *Bridge) GetMessagesForChat(chatID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	db, err := b.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT COALESCE(id, ''), chat_id, COALESCE(sender_name, ''),
		       COALESCE(content, ''), COALESCE(is_from_bot, 0), COALESCE(timestamp, '')
		FROM messages WHERE chat_id = ? ORDER BY timestamp DESC LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query messages: %w", err)
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChatID, &m.SenderName, &m.Content, &m.IsFromBot, &m.Timestamp); err != nil {
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse so oldest is first
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// SendMessage sends a message to a microclaw chat by inserting directly into the DB.
// Metadata is prepended as context, including instructions to use `cs api` CLI directly.
func (b *Bridge) SendMessage(chatID int64, text string, meta *MessageMeta) error {
	db, err := b.openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	// Build content with metadata context
	content := text
	if meta != nil {
		var parts []string
		if meta.RepoPath != "" {
			parts = append(parts, fmt.Sprintf("repo: %s", meta.RepoPath))
		}
		if meta.Branch != "" {
			parts = append(parts, fmt.Sprintf("branch: %s", meta.Branch))
		}
		if meta.Program != "" {
			parts = append(parts, fmt.Sprintf("program: %s", meta.Program))
		}
		if len(parts) > 0 {
			content = fmt.Sprintf("[%s]\n[tools: use `cs api` commands directly for session/task management]\n%s",
				strings.Join(parts, " | "), text)
		}
	}

	msgID := fmt.Sprintf("cs-bridge-%d-%s", time.Now().UnixMilli(), randomString(6))
	now := time.Now().UTC().Format(time.RFC3339)

	_, err = db.Exec(
		`INSERT INTO messages (id, chat_id, sender_name, content, is_from_bot, timestamp) VALUES (?, ?, 'Claude Squad', ?, 0, ?)`,
		msgID, chatID, content, now,
	)
	if err != nil {
		return fmt.Errorf("failed to insert message: %w", err)
	}

	// Update last_message_time on the chat
	_, _ = db.Exec(`UPDATE chats SET last_message_time = ? WHERE chat_id = ?`, now, chatID)

	return nil
}

// Status returns a summary of the microclaw instance.
func (b *Bridge) Status() (string, error) {
	db, err := b.openDB()
	if err != nil {
		return "", err
	}
	defer db.Close()

	var chats, messages int
	_ = db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&chats)
	_ = db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages)

	var tasks int
	_ = db.QueryRow("SELECT COUNT(*) FROM scheduled_tasks WHERE status = 'active'").Scan(&tasks)

	return fmt.Sprintf("Chats: %d | Messages: %d | Active tasks: %d", chats, messages, tasks), nil
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}
