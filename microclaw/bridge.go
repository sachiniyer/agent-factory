package microclaw

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// MessageMeta contains metadata attached to messages sent from agent-factory.
type MessageMeta struct {
	RepoPath string `json:"repo_path,omitempty"`
	RepoID   string `json:"repo_id,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Program  string `json:"program,omitempty"`
}

// Bridge communicates with a running microclaw instance via its Web API (for sending)
// and SQLite database (for reading).
type Bridge struct {
	MicroClawDir string

	apiBaseURL string
	httpClient *http.Client
	csrfToken  string
	password   string
	authMu     sync.Mutex
}

// NewBridge creates a new Bridge pointing at the given microclaw directory.
// If dir is empty, it defaults to ~/.microclaw.
func NewBridge(dir string) *Bridge {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".microclaw")
	}

	jar, _ := cookiejar.New(nil) // nil options never errors

	password := os.Getenv("MICROCLAW_PASSWORD")
	if password == "" {
		password = "helloworld"
	}

	return &Bridge{
		MicroClawDir: dir,
		apiBaseURL:   "http://localhost:10961",
		httpClient: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
		},
		password: password,
	}
}

// SetAPIURL sets the base URL for the microclaw Web API.
func (b *Bridge) SetAPIURL(url string) {
	b.apiBaseURL = url
}

func (b *Bridge) dbPath() string {
	return filepath.Join(b.MicroClawDir, "runtime", "microclaw.db")
}

// Available returns true if the microclaw Web API is reachable.
func (b *Bridge) Available() bool {
	resp, err := b.httpClient.Get(b.apiBaseURL + "/api/auth/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200 || resp.StatusCode == 401
}

// login authenticates with the microclaw Web API and stores the CSRF token.
func (b *Bridge) login() error {
	body, err := json.Marshal(map[string]string{"password": b.password})
	if err != nil {
		return fmt.Errorf("failed to marshal login body: %w", err)
	}
	resp, err := b.httpClient.Post(
		b.apiBaseURL+"/api/auth/login",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("login failed (status %d), could not read body: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("login failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode login response: %w", err)
	}
	b.csrfToken = result.CSRFToken
	return nil
}

// ensureAuth checks auth status and re-logins if needed.
func (b *Bridge) ensureAuth() error {
	b.authMu.Lock()
	defer b.authMu.Unlock()

	resp, err := b.httpClient.Get(b.apiBaseURL + "/api/auth/status")
	if err != nil {
		return b.login()
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		var status struct {
			Authenticated bool `json:"authenticated"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&status); err == nil && status.Authenticated {
			return nil
		}
	}
	return b.login()
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

// SendMessage sends a message to microclaw via the Web API.
// The message is posted to /api/send_stream which triggers immediate LLM processing.
// Responses appear in the DB and are picked up by the TUI's poll loop.
func (b *Bridge) SendMessage(text string, meta *MessageMeta) error {
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
			content = fmt.Sprintf("[%s]\n[tools: use `af api` commands directly for session/task management]\n%s",
				strings.Join(parts, " | "), text)
		}
	}

	return b.sendViaAPI(content)
}

// sendViaAPI posts the message to microclaw's Web API.
func (b *Bridge) sendViaAPI(content string) error {
	if err := b.ensureAuth(); err != nil {
		return fmt.Errorf("auth failed: %w", err)
	}

	body, err := json.Marshal(map[string]string{"message": content})
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}
	req, err := http.NewRequest("POST", b.apiBaseURL+"/api/send_stream", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if b.csrfToken != "" {
		req.Header.Set("X-CSRF-Token", b.csrfToken)
	}

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request failed: %w", err)
	}

	// Retry once on 401 (session expired)
	if resp.StatusCode == 401 {
		resp.Body.Close()
		if err := b.login(); err != nil {
			return fmt.Errorf("re-auth failed: %w", err)
		}
		body, err = json.Marshal(map[string]string{"message": content})
		if err != nil {
			return fmt.Errorf("failed to marshal message on retry: %w", err)
		}
		req, err = http.NewRequest("POST", b.apiBaseURL+"/api/send_stream", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("failed to create retry request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if b.csrfToken != "" {
			req.Header.Set("X-CSRF-Token", b.csrfToken)
		}
		resp, err = b.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("retry send failed: %w", err)
		}
	}

	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("send failed (status %d), could not read body: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("send failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	// Drain the SSE stream in the background so MicroClaw completes the LLM
	// response. The TUI polls the DB for new messages.
	go func() {
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
	}()
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
	if err := db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&chats); err != nil {
		return "", fmt.Errorf("failed to count chats: %w", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages); err != nil {
		return "", fmt.Errorf("failed to count messages: %w", err)
	}

	var tasks int
	if err := db.QueryRow("SELECT COUNT(*) FROM scheduled_tasks WHERE status = 'active'").Scan(&tasks); err != nil {
		return "", fmt.Errorf("failed to count active tasks: %w", err)
	}

	return fmt.Sprintf("Chats: %d | Messages: %d | Active tasks: %d", chats, messages, tasks), nil
}
