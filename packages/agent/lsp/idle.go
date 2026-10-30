package lsp

import "time"

// ensureClient returns a leased client. Every successful caller must release
// it after its last use, including notifications and diagnostic waits.
func (m *Manager) releaseClient(ws *workspace, id string, client *Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ws.clients[id] == client {
		usage := ws.usage[id]
		usage.inUse--
		usage.lastUsed = time.Now()
	}
}

func (m *Manager) acquireCachedClient(ws *workspace, id string) *Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.workspaces[ws.cwd] != ws {
		return nil
	}
	client := ws.clients[id]
	if client != nil {
		ws.usage[id].inUse++
	}
	return client
}

// startSweeperLocked starts at most one worker, lazily on the first server.
func (m *Manager) startSweeperLocked() {
	if m.sweepStop != nil {
		return
	}
	m.sweepStop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				m.sweepIdle(now)
			case <-m.sweepStop:
				return
			}
		}
	}()
}

// sweepIdle accepts an explicit time so expiry tests need no sleeps or clock
// replacement. Retire under the same lock as acquisition; close outside it
// because server callbacks need the manager lock. Keep a starting barrier
// until Close completes so a replacement cannot overlap the retiring server.
func (m *Manager) sweepIdle(now time.Time) {
	type retired struct {
		ws     *workspace
		id     string
		client *Client
		done   chan struct{}
	}
	var closing []retired
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	for _, ws := range m.workspaces {
		for id, client := range ws.clients {
			usage := ws.usage[id]
			seconds := ws.config.Servers[id].IdleTimeoutSeconds
			if seconds < 0 || usage.inUse != 0 {
				continue
			}
			if seconds == 0 {
				seconds = 300
			}
			if now.Sub(usage.lastUsed).Seconds() < float64(seconds) {
				continue
			}
			done := make(chan struct{})
			ws.starting[id] = done
			ws.retiring[client] = true
			delete(ws.clients, id)
			delete(ws.usage, id)
			delete(ws.opened, id)
			ws.statuses[id] = "stopping"
			closing = append(closing, retired{ws, id, client, done})
		}
	}
	m.mu.Unlock()
	for _, value := range closing {
		_ = value.client.Close()
		m.mu.Lock()
		delete(value.ws.retiring, value.client)
		delete(value.ws.diagnostics, value.id)
		delete(value.ws.errors, value.id)
		delete(value.ws.starting, value.id)
		value.ws.statuses[value.id] = "stopped"
		close(value.done)
		m.mu.Unlock()
	}
}
