package events

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Hub struct {
	db      *pgxpool.Pool
	mu      sync.Mutex
	clients map[*Client]struct{}
	ready   chan struct{}
}
type Client struct {
	UserID, SessionID string
	Conn              *websocket.Conn
	send              chan []byte
	ctx               context.Context
	cancel            context.CancelFunc
	once              sync.Once
	hub               *Hub
}
type Event struct {
	ID         int64           `json:"id"`
	Type       string          `json:"type"`
	ChatID     string          `json:"chat_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

func NewHub(db *pgxpool.Pool) *Hub {
	return &Hub{db: db, clients: map[*Client]struct{}{}, ready: make(chan struct{})}
}
func (h *Hub) Ready() <-chan struct{} { return h.ready }

func (h *Hub) Attach(userID, sessionID string, conn *websocket.Conn) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{UserID: userID, SessionID: sessionID, Conn: conn, send: make(chan []byte, 64), ctx: ctx, cancel: cancel, hub: h}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	go c.writeLoop()
	return c
}

func (c *Client) Close() {
	c.once.Do(func() {
		c.cancel()
		_ = c.Conn.CloseNow()
		c.hub.mu.Lock()
		delete(c.hub.clients, c)
		c.hub.mu.Unlock()
	})
}

func (c *Client) Send(payload []byte) bool {
	select {
	case <-c.ctx.Done():
		return false
	case c.send <- payload:
		return true
	default:
		c.Close()
		return false
	}
}

func (c *Client) writeLoop() {
	defer c.Close()
	for {
		select {
		case <-c.ctx.Done():
			return
		case payload := <-c.send:
			ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
			err := c.Conn.Write(ctx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (h *Hub) CloseSession(sessionID string) {
	h.mu.Lock()
	list := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		if c.SessionID == sessionID {
			list = append(list, c)
		}
	}
	h.mu.Unlock()
	for _, c := range list {
		c.Close()
	}
}

func (h *Hub) CloseUser(userID string) {
	h.mu.Lock()
	list := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		if c.UserID == userID {
			list = append(list, c)
		}
	}
	h.mu.Unlock()
	for _, c := range list {
		c.Close()
	}
}

func (h *Hub) SendToSession(sessionID string, payload []byte) bool {
	h.mu.Lock()
	clients := make([]*Client, 0, 1)
	for c := range h.clients {
		if c.SessionID == sessionID {
			clients = append(clients, c)
		}
	}
	h.mu.Unlock()
	sent := false
	for _, c := range clients {
		if c.Send(payload) {
			sent = true
		}
	}
	return sent
}

func (h *Hub) HasSession(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.SessionID == sessionID {
			return true
		}
	}
	return false
}

func (h *Hub) Run(ctx context.Context) error {
	const overlap int64 = 1000
	var last int64
	if err := h.db.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM events`).Scan(&last); err != nil {
		return err
	}
	seen := map[int64]struct{}{}
	floor := last - overlap
	if floor < 0 {
		floor = 0
	}
	initial, err := h.load(ctx, floor, 2000)
	if err != nil {
		return err
	}
	for _, ev := range initial {
		seen[ev.ID] = struct{}{}
	}
	close(h.ready)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	authTicker := time.NewTicker(30 * time.Second)
	defer authTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-authTicker.C:
			h.revalidate(ctx)
			continue
		case <-ticker.C:
		}
		for {
			floor := last - overlap
			if floor < 0 {
				floor = 0
			}
			batch, err := h.load(ctx, floor, 2000)
			if err != nil || len(batch) == 0 {
				break
			}
			previousLast := last
			for _, ev := range batch {
				if _, exists := seen[ev.ID]; !exists {
					h.dispatch(ctx, ev)
					seen[ev.ID] = struct{}{}
				}
				if ev.ID > last {
					last = ev.ID
				}
			}
			floor = last - overlap
			for id := range seen {
				if id <= floor {
					delete(seen, id)
				}
			}
			if len(batch) < 2000 || last == previousLast {
				break
			}
		}
	}
}

func (h *Hub) revalidate(ctx context.Context) {
	h.mu.Lock()
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		var valid bool
		err := h.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.id=$1 AND s.user_id=$2 AND s.revoked_at IS NULL AND s.expires_at>now() AND NOT u.disabled)`, c.SessionID, c.UserID).Scan(&valid)
		if err != nil || !valid {
			c.Close()
		}
	}
}

func (h *Hub) load(ctx context.Context, after int64, limit int) ([]Event, error) {
	rows, err := h.db.Query(ctx, `SELECT id,chat_id::text,kind,payload,created_at FROM events WHERE id>$1 AND chat_id IS NOT NULL ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Event{}
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.ID, &ev.ChatID, &ev.Type, &ev.Data, &ev.OccurredAt); err != nil {
			return nil, err
		}
		result = append(result, ev)
	}
	return result, rows.Err()
}

func (h *Hub) dispatch(ctx context.Context, ev Event) {
	h.mu.Lock()
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	for _, c := range clients {
		active, allowed := h.canReceive(ctx, c, ev.ChatID)
		if !active {
			c.Close()
			continue
		}
		if allowed {
			c.Send(payload)
		}
	}
}

func (h *Hub) canReceive(ctx context.Context, c *Client, chatID string) (bool, bool) {
	var active, allowed bool
	err := h.db.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.id=$1 AND s.user_id=$2 AND s.revoked_at IS NULL AND s.expires_at>now() AND NOT u.disabled),
		EXISTS(SELECT 1 FROM chat_members cm JOIN chats ch ON ch.id=cm.chat_id WHERE cm.chat_id=$3 AND cm.user_id=$2 AND ch.deleted_at IS NULL)`, c.SessionID, c.UserID, chatID).Scan(&active, &allowed)
	if err != nil {
		return false, false
	}
	return active, allowed
}

func (h *Hub) Replay(ctx context.Context, c *Client, after int64) error {
	if after < 0 {
		return nil
	}
	rows, err := h.db.Query(ctx, `SELECT e.id,e.chat_id::text,e.kind,e.payload,e.created_at FROM events e JOIN chat_members cm ON cm.chat_id=e.chat_id JOIN chats ch ON ch.id=e.chat_id WHERE e.id>$1 AND cm.user_id=$2 AND ch.deleted_at IS NULL ORDER BY e.id LIMIT 100`, after, c.UserID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.ID, &ev.ChatID, &ev.Type, &ev.Data, &ev.OccurredAt); err != nil {
			return err
		}
		active, allowed := h.canReceive(ctx, c, ev.ChatID)
		if !active {
			c.Close()
			return context.Canceled
		}
		if !allowed {
			continue
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if !c.Send(payload) {
			return context.Canceled
		}
	}
	return rows.Err()
}
