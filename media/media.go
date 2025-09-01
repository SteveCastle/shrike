package media

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// MediaItem represents a row from the media table
type MediaItem struct {
	Path          string         `json:"path"`
	Description   sql.NullString `json:"description"`
	Size          sql.NullInt64  `json:"size"`
	Hash          sql.NullString `json:"hash"`
	Width         sql.NullInt64  `json:"width"`
	Height        sql.NullInt64  `json:"height"`
	FormattedSize string         `json:"-"`
	Tags          []MediaTag     `json:"tags"`
	Exists        bool           `json:"exists"`
}

// MediaTag represents a tag with its category
type MediaTag struct {
	Label    string `json:"label"`
	Category string `json:"category"`
}

// MarshalJSON implements custom JSON marshaling for MediaItem
func (m MediaItem) MarshalJSON() ([]byte, error) {
	type Alias MediaItem
	return json.Marshal(&struct {
		*Alias
		Description *string `json:"description"`
		Size        *int64  `json:"size"`
		Hash        *string `json:"hash"`
		Width       *int64  `json:"width"`
		Height      *int64  `json:"height"`
	}{
		Alias: (*Alias)(&m),
		Description: func() *string {
			if m.Description.Valid {
				return &m.Description.String
			} else {
				return nil
			}
		}(),
		Size: func() *int64 {
			if m.Size.Valid {
				return &m.Size.Int64
			} else {
				return nil
			}
		}(),
		Hash: func() *string {
			if m.Hash.Valid {
				return &m.Hash.String
			} else {
				return nil
			}
		}(),
		Width: func() *int64 {
			if m.Width.Valid {
				return &m.Width.Int64
			} else {
				return nil
			}
		}(),
		Height: func() *int64 {
			if m.Height.Valid {
				return &m.Height.Int64
			} else {
				return nil
			}
		}(),
	})
}

// TemplateData represents data for the media template
type TemplateData struct {
	MediaItems  []MediaItem `json:"media_items"`
	Offset      int         `json:"offset"`
	HasMore     bool        `json:"has_more"`
	SearchQuery string      `json:"search_query"`
}

// APIResponse represents the JSON response for the API endpoint
type APIResponse struct {
	Items   []MediaItem `json:"items"`
	HasMore bool        `json:"has_more"`
}

// SearchCondition represents a single search condition
type SearchCondition struct {
	Column   string
	Operator string
	Value    string
	Negate   bool
}

// SearchQuery represents a complete search query with conditions and logic
type SearchQuery struct {
	Conditions []SearchCondition
	Logic      []string // AND, OR between conditions
}

// formatBytes converts bytes to human readable format
func FormatBytes(bytes int64) string {
	if bytes == 0 {
		return "0 B"
	}
	const unit = 1024
	sizes := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	b := float64(bytes)
	for b >= unit && i < len(sizes)-1 {
		b /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", b, sizes[i])
}

// CheckFileExists checks if a file exists at the given path
// Returns true if the file exists, false otherwise
func CheckFileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

// FileExistenceResult holds the result of a file existence check
type FileExistenceResult struct {
	Path   string
	Exists bool
}

// CheckFilesExistConcurrent checks file existence for multiple paths concurrently
// This is more efficient than checking files sequentially when dealing with many files
func CheckFilesExistConcurrent(paths []string) map[string]bool {
	if len(paths) == 0 {
		return make(map[string]bool)
	}

	results := make(chan FileExistenceResult, len(paths))
	var wg sync.WaitGroup

	// Launch goroutines to check file existence
	for _, path := range paths {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			exists := CheckFileExists(p)
			results <- FileExistenceResult{Path: p, Exists: exists}
		}(path)
	}

	// Close results channel when all goroutines are done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	existenceMap := make(map[string]bool)
	for result := range results {
		existenceMap[result.Path] = result.Exists
	}

	return existenceMap
}

// parseSearchQuery parses a search query string into structured conditions
// Format examples:
//
//	path:"video.mp4" AND size:>1000000
//	description:"cat" OR path:"*.jpg" AND NOT size:<100
//	size:>1000000 AND size:<10000000
//	tag:"landscape" AND category:"nature"
//	NOT tag:"portrait" AND category:"animals"
//	exists:true AND tag:"landscape"
//	exists:false OR size:>1000000
//	pathdir:"/some/directory/" (searches only in directory, not subdirectories)
func parseSearchQuery(query string) (*SearchQuery, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}

	sq := &SearchQuery{}

	// Basic regex to match conditions and logic operators
	conditionRegex := regexp.MustCompile(`(NOT\s+)?(\w+):((?:"[^"]*")|(?:[^\s]+))`)
	logicRegex := regexp.MustCompile(`\s+(AND|OR)\s+`)

	// Find all conditions
	matches := conditionRegex.FindAllStringSubmatch(query, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no valid conditions found")
	}

	// Find all logic operators
	logicMatches := logicRegex.FindAllString(query, -1)

	for _, match := range matches {
		condition := SearchCondition{
			Negate: strings.TrimSpace(match[1]) == "NOT",
			Column: strings.ToLower(match[2]),
			Value:  strings.Trim(match[3], `"`),
		}

		// Special handling for pathdir operator - always use LIKE for directory matching
		if condition.Column == "pathdir" {
			condition.Operator = "PATHDIR"
			// Detect and normalize path separator
			pathSep := "/"
			if strings.Contains(condition.Value, "\\") {
				pathSep = "\\"
			}

			// Ensure the path ends with the correct separator
			if !strings.HasSuffix(condition.Value, pathSep) {
				condition.Value += pathSep
			}

			// Store the separator in the value for later use
			// We'll use a special marker to indicate the separator
			if pathSep == "\\" {
				condition.Value = "WIN:" + condition.Value
			} else {
				condition.Value = "UNIX:" + condition.Value
			}
		} else {
			// Determine operator for other columns
			if strings.HasPrefix(condition.Value, ">") {
				condition.Operator = ">"
				condition.Value = condition.Value[1:]
			} else if strings.HasPrefix(condition.Value, "<") {
				condition.Operator = "<"
				condition.Value = condition.Value[1:]
			} else if strings.HasPrefix(condition.Value, ">=") {
				condition.Operator = ">="
				condition.Value = condition.Value[2:]
			} else if strings.HasPrefix(condition.Value, "<=") {
				condition.Operator = "<="
				condition.Value = condition.Value[2:]
			} else if strings.Contains(condition.Value, "*") || strings.Contains(condition.Value, "%") {
				condition.Operator = "LIKE"
				condition.Value = strings.ReplaceAll(condition.Value, "*", "%")
			} else {
				condition.Operator = "="
			}
		}

		sq.Conditions = append(sq.Conditions, condition)
	}

	// Parse logic operators
	for _, logic := range logicMatches {
		sq.Logic = append(sq.Logic, strings.TrimSpace(logic))
	}

	return sq, nil
}

// buildWhereClause converts SearchQuery to SQL WHERE clause and any needed JOINs
// Returns: whereClause, sqlArgs, needsTagJoin, existsConditions
func buildWhereClause(sq *SearchQuery) (string, []interface{}, bool, []SearchCondition) {
	if sq == nil || len(sq.Conditions) == 0 {
		return "", nil, false, nil
	}

	var clauses []string
	var args []interface{}
	var needsTagJoin bool
	var existsConditions []SearchCondition

	for i, condition := range sq.Conditions {
		var clause string
		var columnName string

		// Handle exists conditions separately - don't add to SQL
		if condition.Column == "exists" {
			existsConditions = append(existsConditions, condition)
			continue
		}

		// Map column names to database columns or handle special cases
		switch condition.Column {
		case "path":
			columnName = "m.path"
		case "tags":
			// Support querying media with no tags via tags:none
			val := strings.ToLower(strings.TrimSpace(condition.Value))
			if val == "none" {
				if condition.Negate {
					// NOT tags:none => items having at least one tag
					clause = "EXISTS (\n\t\t\t\t\tSELECT 1 FROM media_tag_by_category mtbc WHERE mtbc.media_path = m.path\n\t\t\t\t)"
				} else {
					// tags:none => items with zero tags
					clause = "NOT EXISTS (\n\t\t\t\t\tSELECT 1 FROM media_tag_by_category mtbc WHERE mtbc.media_path = m.path\n\t\t\t\t)"
				}
			} else {
				// Unknown tags:* value, skip condition
				continue
			}
		case "pathdir":
			// Special handling for pathdir - search in directory but not subdirectories
			// Extract path separator from prefixed format
			var pathSep string
			var actualPath string

			if strings.HasPrefix(condition.Value, "WIN:") {
				pathSep = "\\"
				actualPath = condition.Value[4:] // Remove "WIN:" prefix
			} else if strings.HasPrefix(condition.Value, "UNIX:") {
				pathSep = "/"
				actualPath = condition.Value[5:] // Remove "UNIX:" prefix
			} else {
				// Fallback - shouldn't happen with proper parsing
				pathSep = "/"
				actualPath = condition.Value
			}

			if condition.Negate {
				// For NOT pathdir, exclude items in the specified directory
				clause = "NOT (m.path LIKE ? AND m.path NOT LIKE ?)"
				args = append(args, actualPath+"%", actualPath+"%"+pathSep+"%")
			} else {
				// For pathdir, include items in the specified directory but not subdirectories
				clause = "(m.path LIKE ? AND m.path NOT LIKE ?)"
				args = append(args, actualPath+"%", actualPath+"%"+pathSep+"%")
			}
		case "description":
			columnName = "m.description"
		case "size":
			columnName = "m.size"
		case "hash":
			columnName = "m.hash"
		case "width":
			columnName = "m.width"
		case "height":
			columnName = "m.height"
		case "tagcount":
			// Filter by number of tags associated with a media item
			// Build a correlated subquery counting tags for this media path
			op := condition.Operator
			if op == "LIKE" {
				// LIKE doesn't make sense for numeric comparison; treat as equality
				op = "="
			}
			countExpr := `(SELECT COUNT(*) FROM media_tag_by_category mtbc WHERE mtbc.media_path = m.path)`
			// Parse numeric value
			if val, err := strconv.ParseInt(condition.Value, 10, 64); err == nil {
				clause = fmt.Sprintf("%s %s ?", countExpr, op)
				args = append(args, val)
				if condition.Negate {
					clause = "NOT (" + clause + ")"
				}
			} else {
				// Invalid numeric, skip this condition
				continue
			}
		case "tag":
			if condition.Negate {
				// For NOT tag searches, use NOT EXISTS subquery
				clause = fmt.Sprintf(`NOT EXISTS (
					SELECT 1 FROM media_tag_by_category mtbc 
					WHERE mtbc.media_path = m.path AND mtbc.tag_label %s ?
				)`, condition.Operator)
			} else {
				// For positive tag searches, we'll need a JOIN
				needsTagJoin = true
				clause = fmt.Sprintf("mtbc.tag_label %s ?", condition.Operator)
			}
			args = append(args, condition.Value)
		case "category":
			if condition.Negate {
				// For NOT category searches, use NOT EXISTS subquery
				clause = fmt.Sprintf(`NOT EXISTS (
					SELECT 1 FROM media_tag_by_category mtbc 
					WHERE mtbc.media_path = m.path AND mtbc.category_label %s ?
				)`, condition.Operator)
			} else {
				// For positive category searches, we'll need a JOIN
				needsTagJoin = true
				clause = fmt.Sprintf("mtbc.category_label %s ?", condition.Operator)
			}
			args = append(args, condition.Value)
		default:
			continue // Skip unknown columns
		}

		// Handle regular media table columns (excluding pathdir which is handled above)
		if condition.Column == "path" || condition.Column == "description" || condition.Column == "size" || condition.Column == "hash" || condition.Column == "width" || condition.Column == "height" {
			// Handle nullable columns
			if condition.Column == "description" || condition.Column == "hash" || condition.Column == "width" || condition.Column == "height" {
				if condition.Operator == "=" && condition.Value == "" {
					clause = fmt.Sprintf("%s IS NULL", columnName)
				} else {
					clause = fmt.Sprintf("%s IS NOT NULL AND %s %s ?", columnName, columnName, condition.Operator)
					args = append(args, condition.Value)
				}
			} else {
				clause = fmt.Sprintf("%s %s ?", columnName, condition.Operator)

				// Convert numeric values to integers
				if condition.Column == "size" || condition.Column == "width" || condition.Column == "height" {
					if val, err := strconv.ParseInt(condition.Value, 10, 64); err == nil {
						args = append(args, val)
					} else {
						continue // Skip invalid numeric values
					}
				} else {
					args = append(args, condition.Value)
				}
			}

			if condition.Negate {
				clause = "NOT (" + clause + ")"
			}
		}

		clauses = append(clauses, clause)

		// Add logic operator if not the last condition
		if i < len(sq.Conditions)-1 && i < len(sq.Logic) {
			clauses = append(clauses, sq.Logic[i])
		}
	}

	if len(clauses) == 0 {
		return "", nil, false, existsConditions
	}

	return "WHERE " + strings.Join(clauses, " "), args, needsTagJoin, existsConditions
}

// GetTags fetches all tags for a list of media paths
func GetTags(db *sql.DB, mediaPaths []string) (map[string][]MediaTag, error) {
	if len(mediaPaths) == 0 {
		return make(map[string][]MediaTag), nil
	}

	// Create placeholders for IN clause
	placeholders := make([]string, len(mediaPaths))
	args := make([]interface{}, len(mediaPaths))
	for i, path := range mediaPaths {
		placeholders[i] = "?"
		args[i] = path
	}

	query := fmt.Sprintf(`
		SELECT media_path, tag_label, category_label 
		FROM media_tag_by_category 
		WHERE media_path IN (%s)
		ORDER BY media_path, category_label, tag_label
	`, strings.Join(placeholders, ","))

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tagMap := make(map[string][]MediaTag)
	for rows.Next() {
		var mediaPath, tagLabel, categoryLabel string
		if err := rows.Scan(&mediaPath, &tagLabel, &categoryLabel); err != nil {
			return nil, err
		}

		tag := MediaTag{
			Label:    tagLabel,
			Category: categoryLabel,
		}

		tagMap[mediaPath] = append(tagMap[mediaPath], tag)
	}

	return tagMap, nil
}

// evaluateExistsConditions checks if an item matches the exists conditions
func evaluateExistsConditions(item MediaItem, conditions []SearchCondition) bool {
	if len(conditions) == 0 {
		return true // No exists conditions, so item matches
	}

	for _, condition := range conditions {
		if condition.Column != "exists" {
			continue
		}

		// Parse the expected exists value
		expectedExists := strings.ToLower(condition.Value) == "true"

		// Apply negation if present
		if condition.Negate {
			expectedExists = !expectedExists
		}

		// Check if item matches the condition
		if item.Exists != expectedExists {
			return false // At least one condition doesn't match
		}
	}

	return true // All exists conditions match
}

// GetItems fetches media items from the database with pagination and search
func GetItems(db *sql.DB, offset, limit int, searchQuery string) ([]MediaItem, bool, error) {
	baseQuery := `SELECT DISTINCT m.path, m.description, m.size, m.hash, m.width, m.height FROM media m`
	var joinClause string
	orderBy := ` ORDER BY m.path`

	// Parse search query if provided
	sq, err := parseSearchQuery(searchQuery)
	if err != nil {
		// If parsing fails, ignore search and return all results
		log.Printf("Search query parsing failed: %v", err)
		sq = nil
	}

	// Build WHERE clause and check if we need tag joins
	whereClause, whereArgs, needsTagJoin, existsConditions := buildWhereClause(sq)

	// Add JOIN if needed for tag/category searches
	if needsTagJoin {
		joinClause = ` JOIN media_tag_by_category mtbc ON m.path = mtbc.media_path`
	}

	// If there are exists conditions, we need to implement existence-aware pagination
	if len(existsConditions) > 0 {
		return getItemsWithExistenceFilter(db, baseQuery, joinClause, whereClause, whereArgs, orderBy, offset, limit, existsConditions)
	}

	// Standard pagination when no exists conditions
	limitClause := ` LIMIT ? OFFSET ?`
	var query string
	var args []interface{}

	// Construct full query
	if whereClause != "" {
		query = baseQuery + joinClause + " " + whereClause + orderBy + limitClause
		args = append(whereArgs, limit+1, offset)
	} else {
		query = baseQuery + orderBy + limitClause
		args = []interface{}{limit + 1, offset}
	}

	rows, err := db.Query(query, args...) // Query one extra to check if there are more
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var items []MediaItem
	var mediaPaths []string
	for rows.Next() {
		var item MediaItem
		err := rows.Scan(&item.Path, &item.Description, &item.Size, &item.Hash, &item.Width, &item.Height)
		if err != nil {
			return nil, false, err
		}

		// Handle nullable size field
		if item.Size.Valid {
			item.FormattedSize = FormatBytes(item.Size.Int64)
		} else {
			item.FormattedSize = "Unknown"
		}

		items = append(items, item)
		mediaPaths = append(mediaPaths, item.Path)
	}

	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]           // Remove the extra item
		mediaPaths = mediaPaths[:limit] // Also trim the paths list
	}

	// Fetch tags for all media items
	tagMap, err := GetTags(db, mediaPaths)
	if err != nil {
		log.Printf("Error fetching media tags: %v", err)
		// Continue without tags rather than failing completely
	} else {
		// Populate tags for each item
		for i := range items {
			if tags, exists := tagMap[items[i].Path]; exists {
				items[i].Tags = tags
			} else {
				items[i].Tags = []MediaTag{} // Empty slice instead of nil
			}
		}
	}

	// Check file existence for all media items concurrently
	existenceMap := CheckFilesExistConcurrent(mediaPaths)

	// Populate existence information for each item
	for i := range items {
		if exists, found := existenceMap[items[i].Path]; found {
			items[i].Exists = exists
		} else {
			items[i].Exists = false // Default to false if check failed
		}
	}

	return items, hasMore, nil
}

// GetItemByPath fetches a single media item by its path
func GetItemByPath(db *sql.DB, path string) (*MediaItem, error) {
	query := `SELECT path, description, size, hash, width, height FROM media WHERE path = ?`

	var item MediaItem
	err := db.QueryRow(query, path).Scan(&item.Path, &item.Description, &item.Size, &item.Hash, &item.Width, &item.Height)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // Item not found
		}
		return nil, err
	}

	// Handle nullable size field
	if item.Size.Valid {
		item.FormattedSize = FormatBytes(item.Size.Int64)
	} else {
		item.FormattedSize = "Unknown"
	}

	// Fetch tags for this item
	tagMap, err := GetTags(db, []string{item.Path})
	if err != nil {
		log.Printf("Error fetching tags for item %s: %v", item.Path, err)
		item.Tags = []MediaTag{} // Empty slice instead of nil
	} else {
		if tags, exists := tagMap[item.Path]; exists {
			item.Tags = tags
		} else {
			item.Tags = []MediaTag{} // Empty slice instead of nil
		}
	}

	// Check file existence
	item.Exists = CheckFileExists(item.Path)

	return &item, nil
}

// getItemsWithExistenceFilter handles pagination when exists conditions are present
func getItemsWithExistenceFilter(db *sql.DB, baseQuery, joinClause, whereClause string, whereArgs []interface{}, orderBy string, offset, limit int, existsConditions []SearchCondition) ([]MediaItem, bool, error) {
	const batchSize = 100 // Fetch items in batches
	var allMatchingItems []MediaItem
	var dbOffset = 0

	// Keep fetching batches until we have enough matching items or run out of data
	for len(allMatchingItems) < offset+limit {
		// Construct batch query
		limitClause := ` LIMIT ? OFFSET ?`
		var query string
		var args []interface{}

		if whereClause != "" {
			query = baseQuery + joinClause + " " + whereClause + orderBy + limitClause
			args = append(whereArgs, batchSize, dbOffset)
		} else {
			query = baseQuery + orderBy + limitClause
			args = []interface{}{batchSize, dbOffset}
		}

		rows, err := db.Query(query, args...)
		if err != nil {
			return nil, false, err
		}

		var batchItems []MediaItem
		var batchPaths []string
		for rows.Next() {
			var item MediaItem
			err := rows.Scan(&item.Path, &item.Description, &item.Size, &item.Hash, &item.Width, &item.Height)
			if err != nil {
				rows.Close()
				return nil, false, err
			}

			// Handle nullable size field
			if item.Size.Valid {
				item.FormattedSize = FormatBytes(item.Size.Int64)
			} else {
				item.FormattedSize = "Unknown"
			}

			batchItems = append(batchItems, item)
			batchPaths = append(batchPaths, item.Path)
		}
		rows.Close()

		// If no more items from database, break
		if len(batchItems) == 0 {
			break
		}

		// Fetch tags for batch items
		tagMap, err := GetTags(db, batchPaths)
		if err != nil {
			log.Printf("Error fetching media tags: %v", err)
		} else {
			for i := range batchItems {
				if tags, exists := tagMap[batchItems[i].Path]; exists {
					batchItems[i].Tags = tags
				} else {
					batchItems[i].Tags = []MediaTag{}
				}
			}
		}

		// Check file existence for batch items
		existenceMap := CheckFilesExistConcurrent(batchPaths)
		for i := range batchItems {
			if exists, found := existenceMap[batchItems[i].Path]; found {
				batchItems[i].Exists = exists
			} else {
				batchItems[i].Exists = false
			}
		}

		// Filter batch items by exists conditions
		for _, item := range batchItems {
			if evaluateExistsConditions(item, existsConditions) {
				allMatchingItems = append(allMatchingItems, item)
			}
		}

		// Move to next batch
		dbOffset += batchSize

		// If we got fewer items than batch size, we've reached the end
		if len(batchItems) < batchSize {
			break
		}
	}

	// Apply pagination to filtered results
	totalMatching := len(allMatchingItems)

	// Check if there are more items beyond our current page
	hasMore := totalMatching > offset+limit

	// Extract the requested page
	startIdx := offset
	if startIdx > totalMatching {
		startIdx = totalMatching
	}

	endIdx := startIdx + limit
	if endIdx > totalMatching {
		endIdx = totalMatching
	}

	var resultItems []MediaItem
	if startIdx < endIdx {
		resultItems = allMatchingItems[startIdx:endIdx]
	}

	return resultItems, hasMore, nil
}

// RemovalResult contains the results of a database removal operation
type RemovalResult struct {
	MediaItemsRemoved int64
	TagsRemoved       int64
	ProcessedPaths    []string
	Errors            []error
}

// RemoveItemsFromDB removes media items and their associated tags from the database
// This function is designed to be reusable across different parts of the application
func RemoveItemsFromDB(ctx context.Context, db *sql.DB, paths []string) (*RemovalResult, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection not available")
	}

	if len(paths) == 0 {
		return &RemovalResult{}, nil
	}

	// Clean and validate paths
	var validPaths []string
	for _, path := range paths {
		cleanPath := strings.TrimSpace(path)
		if cleanPath != "" {
			validPaths = append(validPaths, cleanPath)
		}
	}

	if len(validPaths) == 0 {
		return &RemovalResult{}, nil
	}

	result := &RemovalResult{
		ProcessedPaths: validPaths,
	}

	// Process in batches to avoid SQL parameter limits (SQLite limit is typically 999)
	const batchSize = 500 // Use 500 to be safe
	totalMediaRemoved := int64(0)
	totalTagsRemoved := int64(0)

	for i := 0; i < len(validPaths); i += batchSize {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			result.MediaItemsRemoved = totalMediaRemoved
			result.TagsRemoved = totalTagsRemoved
			return result, ctx.Err()
		default:
		}

		end := i + batchSize
		if end > len(validPaths) {
			end = len(validPaths)
		}

		batch := validPaths[i:end]

		// Start a database transaction for this batch
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("failed to start transaction for batch: %w", err))
			result.MediaItemsRemoved = totalMediaRemoved
			result.TagsRemoved = totalTagsRemoved
			return result, err
		}

		// Build parameterized query for this batch
		placeholders := make([]string, len(batch))
		args := make([]interface{}, len(batch))
		for j, path := range batch {
			placeholders[j] = "?"
			args[j] = path
		}

		// First, remove related records from media_tag_by_category table
		tagQuery := fmt.Sprintf(`
			DELETE FROM media_tag_by_category 
			WHERE media_path IN (%s)
		`, strings.Join(placeholders, ","))

		tagResult, err := tx.ExecContext(ctx, tagQuery, args...)
		if err != nil {
			tx.Rollback()
			result.Errors = append(result.Errors, fmt.Errorf("failed to remove media tags for batch: %w", err))
			result.MediaItemsRemoved = totalMediaRemoved
			result.TagsRemoved = totalTagsRemoved
			return result, err
		}

		batchTagsRemoved, _ := tagResult.RowsAffected()
		totalTagsRemoved += batchTagsRemoved

		// Then remove the main media records
		mediaQuery := fmt.Sprintf(`
			DELETE FROM media 
			WHERE path IN (%s)
		`, strings.Join(placeholders, ","))

		mediaResult, err := tx.ExecContext(ctx, mediaQuery, args...)
		if err != nil {
			tx.Rollback()
			result.Errors = append(result.Errors, fmt.Errorf("failed to remove media items for batch: %w", err))
			result.MediaItemsRemoved = totalMediaRemoved
			result.TagsRemoved = totalTagsRemoved
			return result, err
		}

		batchMediaRemoved, _ := mediaResult.RowsAffected()
		totalMediaRemoved += batchMediaRemoved

		// Commit this batch
		if err := tx.Commit(); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("failed to commit transaction for batch: %w", err))
			result.MediaItemsRemoved = totalMediaRemoved
			result.TagsRemoved = totalTagsRemoved
			return result, err
		}
	}

	result.MediaItemsRemoved = totalMediaRemoved
	result.TagsRemoved = totalTagsRemoved
	return result, nil
}

// GetNonExistentItems retrieves all media items from the database that don't exist in the file system
// This function processes all items in batches to avoid memory issues with large databases
func GetNonExistentItems(ctx context.Context, db *sql.DB) ([]string, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection not available")
	}

	const batchSize = 1000 // Process items in batches to manage memory
	var nonExistentPaths []string
	offset := 0

	for {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			return nonExistentPaths, ctx.Err()
		default:
		}

		// Get a batch of media items
		query := `SELECT path FROM media ORDER BY path LIMIT ? OFFSET ?`
		rows, err := db.QueryContext(ctx, query, batchSize, offset)
		if err != nil {
			return nonExistentPaths, fmt.Errorf("failed to query media items: %w", err)
		}

		var batchPaths []string
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return nonExistentPaths, fmt.Errorf("failed to scan media path: %w", err)
			}
			batchPaths = append(batchPaths, path)
		}
		rows.Close()

		// If no more items, we're done
		if len(batchPaths) == 0 {
			break
		}

		// Check file existence for this batch
		existenceMap := CheckFilesExistConcurrent(batchPaths)

		// Collect non-existent paths
		for path, exists := range existenceMap {
			if !exists {
				nonExistentPaths = append(nonExistentPaths, path)
			}
		}

		// Move to next batch
		offset += batchSize

		// If we got fewer items than batch size, we've reached the end
		if len(batchPaths) < batchSize {
			break
		}
	}

	return nonExistentPaths, nil
}

// StreamingCleanupNonExistentItems finds and removes non-existent media items in streaming batches
// This avoids memory issues and provides progress feedback during the operation
func StreamingCleanupNonExistentItems(ctx context.Context, db *sql.DB, progressCallback func(found, removed int)) (*RemovalResult, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection not available")
	}

	const batchSize = 1000      // Process items in batches to manage memory
	const removeBatchSize = 500 // Remove in smaller batches to avoid SQL limits

	result := &RemovalResult{}
	offset := 0
	totalFound := 0
	totalRemoved := 0

	for {
		// Check if context was cancelled
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}

		// Get a batch of media items
		query := `SELECT path FROM media ORDER BY path LIMIT ? OFFSET ?`
		rows, err := db.QueryContext(ctx, query, batchSize, offset)
		if err != nil {
			return result, fmt.Errorf("failed to query media items: %w", err)
		}

		var batchPaths []string
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return result, fmt.Errorf("failed to scan media path: %w", err)
			}
			batchPaths = append(batchPaths, path)
		}
		rows.Close()

		// If no more items, we're done
		if len(batchPaths) == 0 {
			break
		}

		// Check file existence for this batch
		existenceMap := CheckFilesExistConcurrent(batchPaths)

		// Collect non-existent paths from this batch
		var nonExistentPaths []string
		for path, exists := range existenceMap {
			if !exists {
				nonExistentPaths = append(nonExistentPaths, path)
			}
		}

		totalFound += len(nonExistentPaths)

		// Remove non-existent items if any found
		if len(nonExistentPaths) > 0 {
			// Process removals in smaller batches to avoid SQL parameter limits
			for i := 0; i < len(nonExistentPaths); i += removeBatchSize {
				end := i + removeBatchSize
				if end > len(nonExistentPaths) {
					end = len(nonExistentPaths)
				}

				removeBatch := nonExistentPaths[i:end]

				// Remove this batch
				batchResult, err := RemoveItemsFromDB(ctx, db, removeBatch)
				if err != nil {
					result.Errors = append(result.Errors, err)
					return result, err
				}

				// Accumulate results
				result.MediaItemsRemoved += batchResult.MediaItemsRemoved
				result.TagsRemoved += batchResult.TagsRemoved
				result.ProcessedPaths = append(result.ProcessedPaths, batchResult.ProcessedPaths...)
				result.Errors = append(result.Errors, batchResult.Errors...)

				totalRemoved += len(removeBatch)

				// Call progress callback if provided
				if progressCallback != nil {
					progressCallback(totalFound, totalRemoved)
				}
			}
		}

		// Move to next batch
		offset += batchSize

		// If we got fewer items than batch size, we've reached the end
		if len(batchPaths) < batchSize {
			break
		}
	}

	return result, nil
}
