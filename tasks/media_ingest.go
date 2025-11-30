package tasks

import (
	"strings"
	"sync"

	"github.com/stevecastle/shrike/jobqueue"
)

// ingestTask is the main dispatcher for media ingestion
// It routes to the appropriate handler based on input type:
// - Local file paths: scans directories for media files
// - YouTube URLs: uses yt-dlp to download
// - Other HTTP URLs: uses gallery-dl to download
func ingestTask(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	input := strings.TrimSpace(j.Input)

	// Determine the input type and route accordingly
	switch {
	case isHTTPURL(input):
		if isYouTubeURL(input) {
			return ingestYouTubeTask(j, q, mu)
		}
		return ingestGalleryTask(j, q, mu)
	default:
		// Treat as local path
		return ingestLocalTask(j, q, mu)
	}
}

// isHTTPURL checks if the input looks like an HTTP(S) URL
func isHTTPURL(input string) bool {
	lower := strings.ToLower(input)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}
