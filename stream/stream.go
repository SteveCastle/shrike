package stream

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type clientChan chan Message

// We'll keep a global list of clients
var (
	clients   = make(map[clientChan]bool)
	clientsMu sync.Mutex
)

type Message struct {
	Type string `json:"type"`
	Msg  string `json:"msg"`
}

// addClient registers a new client channel
func AddClient(c clientChan) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	clients[c] = true
}

// removeClient removes a client channel
func RemoveClient(c clientChan) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	delete(clients, c)
	close(c)
}

// broadcast sends a message to all registered clients
func Broadcast(msg Message) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for c := range clients {
		// Non-blocking send: if channel is full, skip that client
		select {
		case c <- msg:
		default:
		}
	}
}

// StreamHandler handles the SSE endpoint
func StreamHandler(w http.ResponseWriter, r *http.Request) {
	// Ensure proper SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Recommended for SSE: disable gzip/deflate by removing Content-Encoding
	w.Header().Del("Content-Encoding")

	// Create a message channel for this client
	messageChan := make(chan Message, 1)
	AddClient(messageChan)
	defer RemoveClient(messageChan)

	// The HTTP connection must remain open
	// We'll listen until the client disconnects
	// Or the function returns.
	notify := w.(http.CloseNotifier).CloseNotify()

	// Periodic flush (some clients may need this to ensure they get data)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	// Write a comment line to keep the connection open if no events are sent
	// This isn't strictly necessary, but useful for keeping some proxies alive.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-notify:
			// The client disconnected
			return
		case msg := <-messageChan:
			io.WriteString(w, formatSSEResponse(msg))
			flusher.Flush()
		case <-ticker.C:
			// Send a comment to keep the connection alive
			fmt.Println("Sending keep-alive")
			io.WriteString(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}

func formatSSEResponse(msg Message) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", msg.Type,msg.Msg)
}