package main

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	_ "image/jpeg" // <--- registers JPEG decoder
	"image/png"

	_ "github.com/mattn/go-sqlite3"
	"github.com/nfnt/resize"
	"github.com/schollz/progressbar/v3"
	_ "golang.org/x/image/webp"
)

func main() {
	// CLI Flags
	dbPath := flag.String("db", "", "Path to the SQLite database")
	dirPath := flag.String("dir", ".", "Path to the directory to scan")
	recursive := flag.Bool("r", false, "Recursively scan the directory")
	flag.Parse()

	if *dbPath == "" {
		log.Fatal("You must specify a valid --db path to the SQLite database.")
	}

	// Open or create SQLite database
	db, err := sql.Open("sqlite3", *dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v\n", err)
	}
	defer db.Close()

	// Ensure media table exists (adjust schema as needed)
	err = createTableIfNotExists(db)
	if err != nil {
		log.Fatalf("Failed to create table: %v\n", err)
	}

	ensureWidthHeightColumns(db)

	// 1) Scan the filesystem for media files.
	fmt.Println("Scanning directory for media files...")
	mediaFiles, err := scanMediaFiles(*dirPath, *recursive)
	if err != nil {
		log.Fatalf("Failed to scan directory: %v\n", err)
	}
	fmt.Printf("Found %d media file(s).\n\n", len(mediaFiles))

	// 2) Load existing DB entries
	dbMedia, err := loadMediaFromDB(db, *dirPath)
	if err != nil {
		log.Fatalf("Failed to load media from DB: %v\n", err)
	}

	// 3) Compare the two sets
	existingFiles := make([]string, 0)
	newFiles := make([]string, 0)

	// Build a set for quick membership checks
	dbPaths := make(map[string]bool)
	for p := range dbMedia {
		dbPaths[p] = true
	}

	for _, filePath := range mediaFiles {
		if dbPaths[filePath] {
			existingFiles = append(existingFiles, filePath)
		} else {
			newFiles = append(newFiles, filePath)
		}
	}

	// Find files that exist in DB but no longer on disk
	missingOnDisk := make([]string, 0)
	fsSet := make(map[string]bool)
	for _, f := range mediaFiles {
		fsSet[f] = true
	}
	for pathInDB := range dbMedia {
		if !fsSet[pathInDB] {
			// Possibly also check that the path is in or under dirPath if needed
			if isPathUnderDir(pathInDB, *dirPath, *recursive) {
				missingOnDisk = append(missingOnDisk, pathInDB)
			}
		}
	}

	// 4) Display summary
	fmt.Println("--------------------------------------------------")
	fmt.Printf("Total media files found on disk: %d\n", len(mediaFiles))
	fmt.Printf("Files already in DB: %d\n", len(existingFiles))
	fmt.Printf("New files (not in DB): %d\n", len(newFiles))
	fmt.Printf("Files in DB but missing on disk: %d\n", len(missingOnDisk))
	fmt.Println("--------------------------------------------------")

	// 5) Prompt user for actions
	for {
		fmt.Println("Available actions:")
		fmt.Println("1) generate description")
		fmt.Println("2) generate transcript")
		fmt.Println("3) create hash")
		fmt.Println("4) clean missing files from DB")
		fmt.Println("5) measure dimensions")
		fmt.Println("6) exit")
		fmt.Print("Enter choice: ")

		var choice string
		_, err := fmt.Scanln(&choice)
		if err != nil {
			log.Println("Error reading choice:", err)
			continue
		}

		if choice == "6" {
			fmt.Println("Exiting.")
			break
		}

		// Prompt whether to apply action only to new or also to existing files
		applyFiles := determineApplyFiles()
		switch choice {
		case "1":
			// generate description
			err = generateDescription(db, applyFiles, newFiles, existingFiles)
			if err != nil {
				fmt.Println("Error while generating description:", err)
			}
		case "2":
			// generate transcript (mocked)
			err = generateTranscript(db, applyFiles, newFiles, existingFiles)
			if err != nil {
				fmt.Println("Error while generating transcript:", err)
			}
		case "3":
			// create hash
			err = createHashForFiles(db, applyFiles, newFiles, existingFiles)
			if err != nil {
				fmt.Println("Error while creating hash:", err)
			}
		case "4":
			// clean missing files
			err = cleanMissingFiles(db, missingOnDisk)
			if err != nil {
				fmt.Println("Error while cleaning missing files:", err)
			}
		case "5":
			// measure dimensions
			err = measureDimensions(db, applyFiles, newFiles, existingFiles)
			if err != nil {
				fmt.Println("Error while measuring dimensions:", err)
			}
		default:
			fmt.Println("Unknown choice. Please select a valid option.")
		}
	}
}

// createTableIfNotExists ensures the media table is present.
// Adjust columns as needed for your schema.
func createTableIfNotExists(db *sql.DB) error {
	stmt := `
    CREATE TABLE IF NOT EXISTS media (
        path TEXT PRIMARY KEY,
        description TEXT,
        transcript TEXT,
        hash TEXT,
        size INTEGER
    );
    `
	_, err := db.Exec(stmt)
	return err
}

// scanMediaFiles scans the directory (recursively if specified) for media files
// and returns a slice of matching file paths.
func scanMediaFiles(dir string, recursive bool) ([]string, error) {
	var files []string

	isMedia := func(path string) bool {
		ext := strings.ToLower(filepath.Ext(path))
		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".heic", ".tif", ".tiff",
			".mp4", ".mov", ".avi", ".mkv", ".webm", ".wmv":
			return true
		}
		return false
	}

	walkFn := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && !recursive && path != dir {
			// If not recursive, skip subdirectories
			return filepath.SkipDir
		}
		if !info.IsDir() && isMedia(path) {
			files = append(files, path)
		}
		return nil
	}

	err := filepath.Walk(dir, walkFn)
	if err != nil {
		return nil, err
	}

	return files, nil
}

// loadMediaFromDB loads all media rows into a map keyed by path.
func loadMediaFromDB(db *sql.DB, dirPath string) (map[string]struct{}, error) {
	query := fmt.Sprintf("SELECT path FROM media WHERE path LIKE '%s%%';", dirPath)

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]struct{})
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		result[path] = struct{}{}
	}
	return result, nil
}

// isPathUnderDir checks whether pathInDB is under the specified directory (dirPath).
// If recursive is false, it only matches if pathInDB is exactly in dirPath, not subdirectories.
func isPathUnderDir(pathInDB, dirPath string, recursive bool) bool {
	rel, err := filepath.Rel(dirPath, pathInDB)
	if err != nil {
		return false
	}
	if recursive {
		// If recursive, rel should not go out of the directory (no "../").
		return !strings.HasPrefix(rel, "..")
	}
	// If not recursive, the file must be in the same directory (no path separators).
	return filepath.Dir(pathInDB) == dirPath
}

// determineApplyFiles prompts the user whether we apply an action
// to only new files or to both new and existing files.
func determineApplyFiles() string {
	for {
		fmt.Println("Apply action to:")
		fmt.Println("1) New files only")
		fmt.Println("2) Both new and existing files")
		fmt.Print("Enter choice: ")

		var choice string
		_, err := fmt.Scanln(&choice)
		if err != nil {
			fmt.Println("Error reading input:", err)
			continue
		}
		if choice == "1" {
			return "new"
		} else if choice == "2" {
			return "all"
		} else {
			fmt.Println("Invalid selection. Please choose 1 or 2.")
		}
	}
}

func generateDescription(db *sql.DB, applyScope string, newFiles, existingFiles []string) error {
	var filesToUpdate []string
	if applyScope == "new" {
		filesToUpdate = newFiles
	} else {
		filesToUpdate = append(newFiles, existingFiles...)
	}
	if len(filesToUpdate) == 0 {
		fmt.Println("No files to process for description.")
		return nil
	}

	fmt.Println("Generating description using Ollama llama3.2-vision:latest...")
	bar := progressbar.Default(int64(len(filesToUpdate)), "Describing files")

	stmtUpsert := `
    INSERT INTO media(path, description) 
    VALUES (?, ?)
    ON CONFLICT(path) DO UPDATE SET description=excluded.description;
    `

	for _, mediaPath := range filesToUpdate {
		description, descErr := describeFileWithOllama(mediaPath)
		if descErr != nil {
			log.Printf("Warning: could not describe %s: %v\n", mediaPath, descErr)
			_ = bar.Add(1)
			continue
		}

		// Insert/Update DB record
		_, execErr := db.Exec(stmtUpsert, mediaPath, description)
		if execErr != nil {
			return fmt.Errorf("failed to upsert description for %s: %w", mediaPath, execErr)
		}

		_ = bar.Add(1)
	}
	return nil
}

// describeFileWithOllama determines if it's an image or video, extracts/resizes an image,
// calls Ollama's HTTP API with llama3.2-vision:latest, and returns the text description.
func describeFileWithOllama(mediaPath string) (string, error) {
	ext := strings.ToLower(filepath.Ext(mediaPath))
	var tempImagePath string
	var cleanupPaths []string

	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".bmp" || ext == ".webp" {
		// It's an image
		tempImagePath = mediaPath
	} else {
		// Assume it's a video (or other type). We’ll take a screenshot using ffmpeg.
		// This code uses a screenshot at 1 second in.
		screenshotPath := filepath.Join(os.TempDir(), "ollama_screenshot_"+filepath.Base(mediaPath)+".jpg")
		cleanupPaths = append(cleanupPaths, screenshotPath)

		// Example ffmpeg command:
		// ffmpeg -ss 1 -i <mediaPath> -frames:v 1 -q:v 2 <screenshotPath>
		ffmpegCmd := exec.Command("ffmpeg",
			"-ss", "1",
			"-i", mediaPath,
			"-frames:v", "1",
			"-q:v", "2",
			screenshotPath,
		)
		if err := ffmpegCmd.Run(); err != nil {
			return "", fmt.Errorf("ffmpeg screenshot failed: %w", err)
		}
		tempImagePath = screenshotPath
	}

	// Now we have an image at tempImagePath. We need to ensure it does not exceed 1024 in any dimension.
	resizedPath, err := resizeIfNeeded(tempImagePath)
	if err != nil {
		// Clean up partials if needed
		for _, p := range cleanupPaths {
			_ = os.Remove(p)
		}
		return "", fmt.Errorf("failed to resize image: %w", err)
	}
	if resizedPath != tempImagePath {
		cleanupPaths = append(cleanupPaths, resizedPath)
	}

	// Call Ollama with the image
	description, err := callOllamaVision(resizedPath)
	if err != nil {
		// Clean up partials if needed
		for _, p := range cleanupPaths {
			_ = os.Remove(p)
		}
		return "", fmt.Errorf("ollama call failed: %w", err)
	}

	// Cleanup
	for _, p := range cleanupPaths {
		_ = os.Remove(p)
	}
	return description, nil
}

// resizeIfNeeded loads the image from path, checks dimensions, and if larger than 1024
// in either width or height, resizes it. Returns the path to the (possibly new) image.
func resizeIfNeeded(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Decode the image (with WebP support from side-effect import).
	img, _, err := image.Decode(f)
	if err != nil {
		return "", fmt.Errorf("image decode failed: %w", err)
	}

	// Check original dimensions
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	// Decide whether resizing is required
	needsResize := (width > 1024 || height > 1024)

	var finalImg image.Image
	if needsResize {
		// Maintain aspect ratio; scale the larger dimension to 1024
		var newWidth, newHeight uint
		if width >= height {
			newWidth = 1024
			newHeight = uint(float64(height) * (1024.0 / float64(width)))
		} else {
			newHeight = 1024
			newWidth = uint(float64(width) * (1024.0 / float64(height)))
		}

		// Perform resize
		finalImg = resize.Resize(newWidth, newHeight, img, resize.Lanczos3)
	} else {
		// No resizing needed, but we still must convert to a PNG file
		finalImg = img
	}

	// Write to a temporary PNG file
	tmpName := fmt.Sprintf("ollama_converted_%s.png", filepath.Base(path))
	convertedPath := filepath.Join(os.TempDir(), tmpName)

	out, err := os.Create(convertedPath)
	if err != nil {
		return "", err
	}
	defer out.Close()

	// Encode always to PNG (change if you'd like to conditionally use JPEG)
	if err := png.Encode(out, finalImg); err != nil {
		return "", err
	}

	return convertedPath, nil
}

// callOllamaVision calls the local Ollama server to describe an image
// using the model "llama3.2-vision:latest" and returns the textual description.
//
// This example does a simple JSON POST with base64 data. Adjust to match
// the actual Ollama API for image-based prompts.
func callOllamaVision(imagePath string) (string, error) {
	// 1) Convert image to base64
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return "", fmt.Errorf("could not read image for Ollama: %w", err)
	}
	b64 := base64.StdEncoding.EncodeToString(data)

	// 2) Build the JSON payload to match the Ollama /api/chat format.
	//    Adjust "content" as you see fit (longer instructions or shorter).
	//    The user example is "what is in this image?" but you can alter.
	requestJSON := fmt.Sprintf(`{
      "model": "llama3.2-vision",
	  "stream": false,
        "prompt": "Please Describe this image, paying special attention to the people, the color of hair, clothing, items, text and captions, and actions being performed.",
        "images": ["%s"]
    }`, b64)

	// Create the request
	req, err := http.NewRequest("POST", "http://localhost:11434/api/generate", strings.NewReader(requestJSON))
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 3) Send request to Ollama
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama error: status=%d, body=%s", resp.StatusCode, string(bodyBytes))
	}

	// 4) Read entire response body as text. (Ollama may return JSON or raw text.)
	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response body failed: %w", err)
	}

	// Ollama returns JSON, we want the response property
	var response struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(respData, &response); err != nil {
		return "", fmt.Errorf("could not unmarshal Ollama response: %w", err)
	}

	return response.Response, nil
}

func generateTranscript(db *sql.DB, applyScope string, newFiles, existingFiles []string) error {
	// Decide which files to process
	var filesToUpdate []string
	if applyScope == "new" {
		filesToUpdate = newFiles
	} else {
		filesToUpdate = append(newFiles, existingFiles...)
	}
	if len(filesToUpdate) == 0 {
		fmt.Println("No files to process for transcript.")
		return nil
	}

	fmt.Println("Generating transcript with faster-whisper-xxl...")
	bar := progressbar.Default(int64(len(filesToUpdate)), "Transcribing files")

	// Begin a transaction for DB updates
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Prepare our upsert statement once.
	// This sets transcript=excluded.transcript on conflict (i.e., path already in DB).
	stmtUpsert := `
    INSERT INTO media(path, transcript)
    VALUES (?, ?)
    ON CONFLICT(path) DO UPDATE SET transcript=excluded.transcript;
    `

	for _, filePath := range filesToUpdate {

		// Only run this on video files
		ext := strings.ToLower(filepath.Ext(filePath))
		switch ext {
		case ".mp4", ".mov", ".avi", ".mkv", ".webm", ".wmv":
			// OK
		default:
			// Skip non-video files
			_ = bar.Add(1)
			continue
		}

		// 1) Run the external command on this file
		cmd := exec.Command(
			"faster-whisper-xxl.exe",
			"--beep_off",
			"--output_format=vtt",
			"--output_dir=source",
			filePath, // the input media file
		)

		// Optionally: If "faster-whisper-xxl.exe" is not in PATH, specify a full path:
		// cmd := exec.Command("C:\\path\\to\\faster-whisper-xxl.exe", "--output_format=vtt", ...)

		// 2) Execute the command
		errCmd := cmd.Run()
		if errCmd != nil {
			log.Printf("Warning: error transcribing %s: %v\n", filePath, errCmd)
			// Move on to next file but still add to progress bar
			_ = bar.Add(1)
			continue
		}

		// 3) Figure out where the .vtt file got saved
		//    Usually the tool might produce "source/<base>.vtt" from <base> of your file.
		//    For example, if filePath = "C:\videos\myvideo.mp4", we might get ""C:\videos\myvideo.vtt".
		filepathNoExt := filePath[:len(filePath)-len(filepath.Ext(filePath))]
		vttPath := filepath.Join(filepathNoExt + ".vtt")

		// 4) Read the .vtt file’s contents
		vttData, errRead := readFileAll(vttPath)
		if errRead != nil {
			log.Printf("Warning: could not read VTT file %s: %v\n", vttPath, errRead)
			_ = bar.Add(1)
			continue
		}

		// 5) Insert/Update the transcript in the DB
		_, errExec := tx.Exec(stmtUpsert, filePath, vttData)
		if errExec != nil {
			// If an error occurs, return it so we can rollback
			return errExec
		}

		_ = bar.Add(1)
	}

	// 6) Commit the transaction to save changes
	err = tx.Commit()
	return err
}

// readFileAll is a helper to read an entire file as a string.
// Adjust as needed (memory concerns, etc.).
func readFileAll(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return "", scanErr
	}
	return sb.String(), nil
}

// createHashForFiles calculates the SHA-256 of the first 3MB of the file,
// sets the 'hash' column, and also sets the 'size' column in the DB.
func createHashForFiles(db *sql.DB, applyScope string, newFiles, existingFiles []string) error {
	var filesToUpdate []string
	if applyScope == "new" {
		filesToUpdate = newFiles
	} else {
		filesToUpdate = append(newFiles, existingFiles...)
	}
	if len(filesToUpdate) == 0 {
		fmt.Println("No files to process for hashing.")
		return nil
	}

	fmt.Println("Creating hashes (first 3MB)...")
	bar := progressbar.Default(int64(len(filesToUpdate)), "Hashing files")

	const maxBytes = 3 * 1024 * 1024 // 3MB

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stmtUpsert := `
    INSERT INTO media(path, hash, size) 
    VALUES (?, ?, ?)
    ON CONFLICT(path) DO UPDATE SET hash=excluded.hash, size=excluded.size;
    `

	for _, f := range filesToUpdate {
		fi, statErr := os.Stat(f)
		if statErr != nil {
			// If file doesn't exist or can't stat, skip
			_ = bar.Add(1)
			continue
		}

		size := fi.Size()
		file, openErr := os.Open(f)
		if openErr != nil {
			_ = bar.Add(1)
			continue
		}

		hashVal, hashErr := hashFirstNBytes(file, maxBytes)
		_ = file.Close()
		if hashErr != nil {
			log.Printf("Skipping hashing file %s: %v\n", f, hashErr)
			_ = bar.Add(1)
			continue
		}

		_, execErr := tx.Exec(stmtUpsert, f, hashVal, size)
		if execErr != nil {
			return execErr
		}

		_ = bar.Add(1)
	}

	err = tx.Commit()
	return err
}

// cleanMissingFiles removes DB records for files that no longer exist on disk.
func cleanMissingFiles(db *sql.DB, missingOnDisk []string) error {
	if len(missingOnDisk) == 0 {
		fmt.Println("No missing files to remove.")
		return nil
	}

	// Quick sanity check with user
	fmt.Printf("This will remove %d entries from the DB. Proceed? (y/n): ", len(missingOnDisk))
	var confirm string
	_, err := fmt.Scanln(&confirm)
	if err != nil {
		return err
	}
	if strings.ToLower(confirm) != "y" {
		fmt.Println("Aborting clean action.")
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	delStmt := `DELETE FROM media WHERE path = ?;`
	for _, f := range missingOnDisk {
		_, errExec := tx.Exec(delStmt, f)
		if errExec != nil {
			return errExec
		}
	}
	err = tx.Commit()
	return err
}

func measureDimensions(db *sql.DB, applyScope string, newFiles, existingFiles []string) (err error) {
	var filesToUpdate []string
	if applyScope == "new" {
		filesToUpdate = newFiles
	} else {
		filesToUpdate = append(newFiles, existingFiles...)
	}
	if len(filesToUpdate) == 0 {
		fmt.Println("No files to measure for dimensions.")
		return nil
	}

	fmt.Println("Measuring media dimensions...")
	bar := progressbar.Default(int64(len(filesToUpdate)), "Measuring")

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		// Only roll back if `err` is non-nil
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Upsert statement for width/height
	stmtUpsert := `
        INSERT INTO media(path, width, height)
        VALUES (?, ?, ?)
        ON CONFLICT(path) DO UPDATE SET width=excluded.width, height=excluded.height;
    `

	for _, path := range filesToUpdate {
		ext := strings.ToLower(filepath.Ext(path))

		var width, height int
		switch ext {
		// --- Recognized image extensions:
		case ".jpg", ".jpeg", ".png", ".bmp", ".webp", ".gif", ".tif", ".tiff", ".heic":
			w, h, imgErr := getImageDimensions(path)
			if imgErr != nil {
				log.Printf("Warning: cannot decode image %s: %v\n", path, imgErr)
				_ = bar.Add(1)
				continue
			}
			width, height = w, h

		// --- Recognized video extensions:
		case ".mp4", ".mov", ".avi", ".mkv", ".webm":
			w, h, vidErr := getVideoDimensionsFFProbe(path)
			if vidErr != nil {
				log.Printf("Warning: cannot probe video %s: %v\n", path, vidErr)
				_ = bar.Add(1)
				continue
			}
			width, height = w, h

		// If not in either set, skip
		default:
			_ = bar.Add(1)
			continue
		}

		// Upsert into DB
		_, errExec := tx.Exec(stmtUpsert, path, width, height)
		if errExec != nil {
			err = fmt.Errorf("failed upsert for %s: %w", path, errExec)
			return err
		}

		_ = bar.Add(1)
	}

	err = tx.Commit()
	return err
}

// getImageDimensions opens the file and uses image.DecodeConfig to get width/height.
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

// getVideoDimensionsFFProbe runs ffprobe and parses the width/height of the first video stream.
// Requires ffprobe on PATH.
func getVideoDimensionsFFProbe(path string) (int, int, error) {
	// Example ffprobe call:
	//    ffprobe -v error -select_streams v:0 -show_entries stream=width,height \
	//            -of csv=s=x:p=0 <filename>
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=s=x:p=0",
		path)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}

	// The output should look like e.g. "1920x1080\n"
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

// hashFirstNBytes calculates the SHA-256 hash of the first n bytes of a file.
func hashFirstNBytes(r io.Reader, n int64) (string, error) {
	if n < 0 {
		return "", errors.New("invalid byte count")
	}

	hasher := sha256.New()
	limitReader := io.LimitReader(r, n)

	_, err := io.Copy(hasher, limitReader)
	if err != nil {
		return "", err
	}

	sum := hasher.Sum(nil)
	return hex.EncodeToString(sum), nil
}

func ensureWidthHeightColumns(db *sql.DB) {
	_, _ = db.Exec(`ALTER TABLE media ADD COLUMN width INTEGER;`)
	_, _ = db.Exec(`ALTER TABLE media ADD COLUMN height INTEGER;`)
}
