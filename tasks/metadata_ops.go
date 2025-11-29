package tasks

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/stevecastle/shrike/appconfig"
	"github.com/stevecastle/shrike/jobqueue"
)

// generateDescriptions generates descriptions for media files using Ollama
func generateDescriptions(ctx context.Context, q *jobqueue.Queue, jobID string, filePaths []string, overwrite bool, model string) error {
	// Pre-filter to compute exact candidates and total for progress
	var candidates []string
	for _, filePath := range filePaths {
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: file does not exist: %s", filePath))
			continue
		}
		if !overwrite {
			hasDescription, err := hasExistingMetadata(q.Db, filePath, "description")
			if err != nil {
				log.Printf("Error checking existing description for %s: %v", filePath, err)
				continue
			}
			if hasDescription {
				continue
			}
		}
		candidates = append(candidates, filePath)
	}
	if len(candidates) == 0 {
		q.PushJobStdout(jobID, "Description: 0 files to process")
		return nil
	}
	q.PushJobStdout(jobID, fmt.Sprintf("Description: %d files to process", len(candidates)))

	processed := 0
	for i, filePath := range candidates {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		description, err := describeFileWithOllama(ctx, filePath, model)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to describe %s: %v", filePath, err))
			continue
		}
		if err := updateMediaMetadata(q.Db, filePath, "description", description); err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to update description for %s: %v", filePath, err))
			continue
		}
		processed++
		q.PushJobStdout(jobID, fmt.Sprintf("Description %d/%d: %s", i+1, len(candidates), filepath.Base(filePath)))
	}
	q.PushJobStdout(jobID, fmt.Sprintf("Generated descriptions for %d files", processed))
	return nil
}

// generateTranscripts generates transcripts for video files using faster-whisper
func generateTranscripts(ctx context.Context, q *jobqueue.Queue, jobID string, filePaths []string, overwrite bool) error {
	// Pre-filter to compute exact candidates and total for progress
	var candidates []string
	for _, filePath := range filePaths {
		ext := strings.ToLower(filepath.Ext(filePath))
		switch ext {
		case ".mp4", ".mov", ".avi", ".mkv", ".webm", ".wmv":
		default:
			continue
		}
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: file does not exist: %s", filePath))
			continue
		}
		if !overwrite {
			hasTranscript, err := hasExistingMetadata(q.Db, filePath, "transcript")
			if err != nil {
				log.Printf("Error checking existing transcript for %s: %v", filePath, err)
				continue
			}
			if hasTranscript {
				continue
			}
		}
		candidates = append(candidates, filePath)
	}
	if len(candidates) == 0 {
		q.PushJobStdout(jobID, "Transcript: 0 video files to process")
		return nil
	}
	q.PushJobStdout(jobID, fmt.Sprintf("Transcript: %d video files to process", len(candidates)))

	processed := 0
	for i, filePath := range candidates {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		transcript, err := generateTranscriptWithFasterWhisper(ctx, filePath)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to transcribe %s: %v", filePath, err))
			continue
		}
		if err := updateMediaMetadata(q.Db, filePath, "transcript", transcript); err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to update transcript for %s: %v", filePath, err))
			continue
		}
		processed++
		q.PushJobStdout(jobID, fmt.Sprintf("Transcript %d/%d: %s", i+1, len(candidates), filepath.Base(filePath)))
	}
	q.PushJobStdout(jobID, fmt.Sprintf("Generated transcripts for %d video files", processed))
	return nil
}

// generateHashes generates hashes for media files
func generateHashes(ctx context.Context, q *jobqueue.Queue, jobID string, filePaths []string, overwrite bool) error {
	const maxBytes = 3 * 1024 * 1024
	// Pre-filter to compute exact candidates and total for progress
	var candidates []string
	for _, filePath := range filePaths {
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: file does not exist: %s", filePath))
			continue
		}
		if !overwrite {
			hasHash, err := hasExistingMetadata(q.Db, filePath, "hash")
			if err != nil {
				log.Printf("Error checking existing hash for %s: %v", filePath, err)
				continue
			}
			if hasHash {
				continue
			}
		}
		candidates = append(candidates, filePath)
	}
	if len(candidates) == 0 {
		q.PushJobStdout(jobID, "Hash: 0 files to process")
		return nil
	}
	q.PushJobStdout(jobID, fmt.Sprintf("Hash: %d files to process", len(candidates)))

	processed := 0
	for i, filePath := range candidates {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		fi, err := os.Stat(filePath)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to stat %s: %v", filePath, err))
			continue
		}
		file, err := os.Open(filePath)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to open %s: %v", filePath, err))
			continue
		}
		hashVal, err := hashFirstNBytes(file, maxBytes)
		file.Close()
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to hash %s: %v", filePath, err))
			continue
		}
		stmt := `UPDATE media SET hash = ?, size = ? WHERE path = ?`
		_, err = q.Db.Exec(stmt, hashVal, fi.Size(), filePath)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to update hash for %s: %v", filePath, err))
			continue
		}
		processed++
		q.PushJobStdout(jobID, fmt.Sprintf("Hash %d/%d: %s", i+1, len(candidates), filepath.Base(filePath)))
	}
	q.PushJobStdout(jobID, fmt.Sprintf("Generated hashes for %d files", processed))
	return nil
}

// generateDimensions generates width/height dimensions for media files
func generateDimensions(ctx context.Context, q *jobqueue.Queue, jobID string, filePaths []string, overwrite bool) error {
	// Pre-filter to compute exact candidates and total for progress
	var candidates []string
	for _, filePath := range filePaths {
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: file does not exist: %s", filePath))
			continue
		}
		if !overwrite {
			hasDimensions, err := hasExistingDimensions(q.Db, filePath)
			if err != nil {
				log.Printf("Error checking existing dimensions for %s: %v", filePath, err)
				continue
			}
			if hasDimensions {
				continue
			}
		}
		ext := strings.ToLower(filepath.Ext(filePath))
		switch ext {
		case ".jpg", ".jpeg", ".png", ".bmp", ".webp", ".gif", ".tif", ".tiff", ".heic", ".mp4", ".mov", ".avi", ".mkv", ".webm":
			candidates = append(candidates, filePath)
		}
	}
	if len(candidates) == 0 {
		q.PushJobStdout(jobID, "Dimensions: 0 files to process")
		return nil
	}
	q.PushJobStdout(jobID, fmt.Sprintf("Dimensions: %d files to process", len(candidates)))

	processed := 0
	for i, filePath := range candidates {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		ext := strings.ToLower(filepath.Ext(filePath))
		var width, height int
		var err error
		switch ext {
		case ".jpg", ".jpeg", ".png", ".bmp", ".webp", ".gif", ".tif", ".tiff", ".heic":
			width, height, err = getImageDimensions(filePath)
		case ".mp4", ".mov", ".avi", ".mkv", ".webm":
			width, height, err = getVideoDimensionsFFProbe(filePath)
		default:
			continue
		}
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to get dimensions for %s: %v", filePath, err))
			continue
		}
		_, err = q.Db.Exec(`UPDATE media SET width = ?, height = ? WHERE path = ?`, width, height, filePath)
		if err != nil {
			q.PushJobStdout(jobID, fmt.Sprintf("Warning: failed to update dimensions for %s: %v", filePath, err))
			continue
		}
		processed++
		q.PushJobStdout(jobID, fmt.Sprintf("Dimensions %d/%d: %s", i+1, len(candidates), filepath.Base(filePath)))
	}
	q.PushJobStdout(jobID, fmt.Sprintf("Generated dimensions for %d files", processed))
	return nil
}

func hasExistingMetadata(db *sql.DB, path, metadataType string) (bool, error) {
	query := fmt.Sprintf(`SELECT %s FROM media WHERE path = ?`, metadataType)
	var value sql.NullString
	if err := db.QueryRow(query, path).Scan(&value); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return value.Valid && value.String != "", nil
}

func hasExistingDimensions(db *sql.DB, path string) (bool, error) {
	var width, height sql.NullInt64
	if err := db.QueryRow(`SELECT width, height FROM media WHERE path = ?`, path).Scan(&width, &height); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return width.Valid && height.Valid, nil
}

func updateMediaMetadata(db *sql.DB, path, metadataType, value string) error {
	query := fmt.Sprintf(`UPDATE media SET %s = ? WHERE path = ?`, metadataType)
	_, err := db.Exec(query, value, path)
	return err
}

func describeFileWithOllama(ctx context.Context, mediaPath, model string) (string, error) {
	ext := strings.ToLower(filepath.Ext(mediaPath))
	var tempImagePath string
	var cleanupPaths []string
	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".bmp" || ext == ".webp" {
		tempImagePath = mediaPath
	} else {
		screenshotPath := filepath.Join(os.TempDir(), "ollama_screenshot_"+filepath.Base(mediaPath)+".jpg")
		cleanupPaths = append(cleanupPaths, screenshotPath)
		ffmpegCmd := exec.CommandContext(ctx, "ffmpeg", "-ss", "1", "-i", mediaPath, "-frames:v", "1", "-q:v", "2", "-y", screenshotPath)
		if err := ffmpegCmd.Run(); err != nil {
			return "", fmt.Errorf("ffmpeg screenshot failed: %w", err)
		}
		tempImagePath = screenshotPath
	}
	resizedPath, err := resizeImageIfNeeded(tempImagePath)
	if err != nil {
		for _, p := range cleanupPaths {
			_ = os.Remove(p)
		}
		return "", fmt.Errorf("failed to resize image: %w", err)
	}
	if resizedPath != tempImagePath {
		cleanupPaths = append(cleanupPaths, resizedPath)
	}
	description, err := callOllamaVision(ctx, resizedPath, model)
	if err != nil {
		for _, p := range cleanupPaths {
			_ = os.Remove(p)
		}
		return "", fmt.Errorf("ollama call failed: %w", err)
	}
	for _, p := range cleanupPaths {
		_ = os.Remove(p)
	}
	return description, nil
}

func resizeImageIfNeeded(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return "", fmt.Errorf("image decode failed: %w", err)
	}
	b := img.Bounds()
	if b.Dx() <= 1024 && b.Dy() <= 1024 {
		return path, nil
	}
	convertedPath := filepath.Join(os.TempDir(), fmt.Sprintf("ollama_resized_%s.png", filepath.Base(path)))
	out, err := os.Create(convertedPath)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if err := png.Encode(out, img); err != nil {
		return "", err
	}
	return convertedPath, nil
}

func callOllamaVision(ctx context.Context, imagePath, model string) (string, error) {
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return "", fmt.Errorf("could not read image for Ollama: %w", err)
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	reqJSON := fmt.Sprintf(`{"model":"%s","stream":false,"prompt":%s,"images":["%s"]}`,
		model, strconv.Quote(appconfig.Get().DescribePrompt), b64)
	base := strings.TrimRight(appconfig.Get().OllamaBaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/api/generate", strings.NewReader(reqJSON))
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 600 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama error: status=%d, body=%s", resp.StatusCode, string(body))
	}
	var response struct {
		Response string `json:"response"`
	}
	dataBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body failed: %w", err)
	}
	if err := json.Unmarshal(dataBytes, &response); err != nil {
		return "", fmt.Errorf("could not unmarshal Ollama response: %w", err)
	}
	return response.Response, nil
}

func generateTranscriptWithFasterWhisper(ctx context.Context, filePath string) (string, error) {
	exePath := appconfig.Get().FasterWhisperPath
	if strings.TrimSpace(exePath) == "" {
		exePath = "faster-whisper-xxl.exe"
	}
	cmd := exec.CommandContext(ctx, exePath, "--beep_off", "--output_format=vtt", "--output_dir=source", "--model", "large-v2", filePath)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("faster-whisper-xxl failed: %w", err)
	}
	vttPath := filePath[:len(filePath)-len(filepath.Ext(filePath))] + ".vtt"
	return readFileAll(vttPath)
}

func readFileAll(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var sb strings.Builder
	s := bufio.NewScanner(f)
	for s.Scan() {
		sb.WriteString(s.Text())
		sb.WriteByte('\n')
	}
	if scanErr := s.Err(); scanErr != nil {
		return "", scanErr
	}
	return sb.String(), nil
}

func getImageDimensions(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

func getVideoDimensionsFFProbe(path string) (int, int, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=s=x:p=0", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	dims := strings.Split(strings.TrimSpace(string(out)), "x")
	if len(dims) != 2 {
		return 0, 0, errors.New("unexpected ffprobe output: " + string(out))
	}
	width, wErr := strconv.Atoi(dims[0])
	height, hErr := strconv.Atoi(dims[1])
	if wErr != nil || hErr != nil {
		return 0, 0, fmt.Errorf("failed to parse width/height from: %s", string(out))
	}
	return width, height, nil
}

func hashFirstNBytes(r io.Reader, n int64) (string, error) {
	if n < 0 {
		return "", errors.New("invalid byte count")
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(r, n)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileExistsInDatabase(db *sql.DB, path string) (bool, error) {
	var exists int
	if err := db.QueryRow(`SELECT 1 FROM media WHERE path = ? LIMIT 1`, path).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
