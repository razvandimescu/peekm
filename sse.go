package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// fileEventMessage is used for SSE notifications about file changes
type fileEventMessage struct {
	Type      string `json:"type"` // "file_added", "files_added" or "file_removed"
	Path      string `json:"path"`
	Session   string `json:"session,omitempty"`   // Optional Claude Code session ID
	PlanTitle string `json:"planTitle,omitempty"` // Non-empty for plan file events
	Count     int    `json:"count,omitempty"`     // Files covered by a "files_added" summary
}

// connectionStatusMessage is used for SSE notifications about connection status
type connectionStatusMessage struct {
	Type  string `json:"type"`  // "connection_status"
	Count int    `json:"count"` // Number of active connections
}

// eventRecord stores a single SSE event with ID for replay
type eventRecord struct {
	id   string // Monotonic counter
	data string // JSON message
}

// eventBuffer maintains a circular buffer of recent events for SSE replay
type eventBuffer struct {
	mu      sync.RWMutex
	events  []eventRecord
	counter uint64
	maxSize int
}

// newEventBuffer creates an eventBuffer with specified capacity
func newEventBuffer(maxSize int) *eventBuffer {
	return &eventBuffer{
		events:  make([]eventRecord, 0, maxSize),
		maxSize: maxSize,
	}
}

// add assigns an event ID, stores the event, and returns the ID
func (eb *eventBuffer) add(data string) string {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	eb.counter++
	id := fmt.Sprintf("%d", eb.counter)

	evt := eventRecord{
		id:   id,
		data: data,
	}

	// Circular buffer: if at capacity, remove oldest
	if len(eb.events) >= eb.maxSize {
		eb.events = eb.events[1:]
	}
	eb.events = append(eb.events, evt)

	return id
}

// getAfter returns all events after the specified ID
func (eb *eventBuffer) getAfter(lastID string) []eventRecord {
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	var result []eventRecord
	foundLast := false

	for _, evt := range eb.events {
		if foundLast {
			result = append(result, evt)
		}
		if evt.id == lastID {
			foundLast = true
		}
	}

	return result
}

func serveSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable proxy buffering

	// Verify flusher support early
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("SSE error: ResponseWriter doesn't support flushing")
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	clientChan := make(chan string, 10) // Buffer 10 events to handle bursts

	clientsMutex.Lock()
	clients[clientChan] = true
	clientCount := len(clients)
	clientsMutex.Unlock()

	// Broadcast connection status to all clients
	broadcastConnectionStatus(clientCount)

	defer func() {
		clientsMutex.Lock()
		delete(clients, clientChan)
		clientCount := len(clients)
		clientsMutex.Unlock()
		close(clientChan)

		// Broadcast updated connection status to remaining clients
		broadcastConnectionStatus(clientCount)
	}()

	// Send initial comment to establish connection
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Replay missed events if client reconnected with Last-Event-ID
	lastEventID := r.Header.Get("Last-Event-ID")
	if lastEventID != "" {
		log.Printf("Client reconnected with Last-Event-ID: %s", lastEventID)
		missedEvents := globalEventBuffer.getAfter(lastEventID)
		if len(missedEvents) > 0 {
			log.Printf("Replaying %d missed events", len(missedEvents))
			for _, evt := range missedEvents {
				fmt.Fprintf(w, "id: %s\ndata: %s\n\n", evt.id, evt.data)
			}
			flusher.Flush()
		} else {
			log.Printf("No missed events found after ID %s", lastEventID)
		}
	}

	// Keep connection alive (10s interval < 15s WriteTimeout to prevent disconnections)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case message := <-clientChan:
			// Message already formatted with "id: X\ndata: Y" from notifyClientsWithMessage
			if _, err := fmt.Fprintf(w, "%s\n\n", message); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func notifyClients() {
	notifyClientsWithMessage("reload")
}

func notifyClientsWithMessage(message string) {
	// Assign event ID and add to buffer for replay
	id := globalEventBuffer.add(message)

	clientsMutex.RLock()
	defer clientsMutex.RUnlock()

	// Send with SSE event ID for replay support
	formattedMsg := fmt.Sprintf("id: %s\ndata: %s", id, message)

	for clientChan := range clients {
		select {
		case clientChan <- formattedMsg:
		default:
		}
	}
}

func broadcastConnectionStatus(count int) {
	msg := connectionStatusMessage{
		Type:  "connection_status",
		Count: count,
	}
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Error marshaling connection status: %v", err)
		return
	}
	notifyClientsWithMessage(string(msgBytes))
}
