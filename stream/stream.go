package stream

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type clientChan chan Message

var clients sync.Map // Use sync.Map for high-concurrency scenarios

type Message struct {
	Type string `json:"type"`
	Msg  string `json:"msg"`
}

// addClient registers a new client channel
func AddClient(c clientChan) {
	fmt.Println("Adding client")
	clients.Store(c, true)
}

// removeClient removes a client channel
func RemoveClient(c clientChan) {
	fmt.Println("Removing client")
	clients.Delete(c)
	close(c)
}

// broadcast sends a message to all registered clients
func Broadcast(msg Message) {
	clients.Range(func(key, value any) bool {
		c := key.(clientChan)
		select {
		case c <- msg: // Send message
		default: // Skip if the channel is full
		}
		return true
	})
}

// StreamHandler handles the SSE endpoint
func StreamHandler(w http.ResponseWriter, r *http.Request) {
	// Ensure proper SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Del("Content-Encoding")

	// Create a message channel for this client
	messageChan := make(chan Message, 10) // Use a buffered channel
	AddClient(messageChan)
	defer RemoveClient(messageChan)

	// Get the context of the HTTP request
	ctx := r.Context()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The client disconnected
			return
		case msg := <-messageChan:
			if _, err := io.WriteString(w, formatSSEResponse(msg)); err != nil {
				return // Stop on write error
			}
			flusher.Flush()
		case <-ticker.C:
			// Send a keep-alive comment
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return // Stop on write error
			}
			flusher.Flush()
		}
	}
}

func formatSSEResponse(msg Message) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", msg.Type, msg.Msg)
}
