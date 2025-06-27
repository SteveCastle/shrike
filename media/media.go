package media

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
)

// MediaItem represents a row from the media table
type MediaItem struct {
	Path          string         `json:"path"`
	Description   sql.NullString `json:"description"`
	Size          sql.NullInt64  `json:"size"`
	Hash          sql.NullString `json:"hash"`
	FormattedSize string         `json:"-"`
	Tags          []MediaTag     `json:"tags"`
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

// parseSearchQuery parses a search query string into structured conditions
// Format examples:
//
//	path:"video.mp4" AND size:>1000000
//	description:"cat" OR path:"*.jpg" AND NOT size:<100
//	size:>1000000 AND size:<10000000
//	tag:"landscape" AND category:"nature"
//	NOT tag:"portrait" AND category:"animals"
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

		// Determine operator
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

		sq.Conditions = append(sq.Conditions, condition)
	}

	// Parse logic operators
	for _, logic := range logicMatches {
		sq.Logic = append(sq.Logic, strings.TrimSpace(logic))
	}

	return sq, nil
}

// buildWhereClause converts SearchQuery to SQL WHERE clause and any needed JOINs
func buildWhereClause(sq *SearchQuery) (string, []interface{}, bool) {
	if sq == nil || len(sq.Conditions) == 0 {
		return "", nil, false
	}

	var clauses []string
	var args []interface{}
	var needsTagJoin bool

	for i, condition := range sq.Conditions {
		var clause string
		var columnName string

		// Map column names to database columns or handle special cases
		switch condition.Column {
		case "path":
			columnName = "m.path"
		case "description":
			columnName = "m.description"
		case "size":
			columnName = "m.size"
		case "hash":
			columnName = "m.hash"
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

		// Handle regular media table columns
		if condition.Column == "path" || condition.Column == "description" || condition.Column == "size" || condition.Column == "hash" {
			// Handle nullable columns
			if condition.Column == "description" || condition.Column == "hash" {
				if condition.Operator == "=" && condition.Value == "" {
					clause = fmt.Sprintf("%s IS NULL", columnName)
				} else {
					clause = fmt.Sprintf("%s IS NOT NULL AND %s %s ?", columnName, columnName, condition.Operator)
					args = append(args, condition.Value)
				}
			} else {
				clause = fmt.Sprintf("%s %s ?", columnName, condition.Operator)

				// Convert size values to integers
				if condition.Column == "size" {
					if val, err := strconv.ParseInt(condition.Value, 10, 64); err == nil {
						args = append(args, val)
					} else {
						continue // Skip invalid size values
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
		return "", nil, false
	}

	return "WHERE " + strings.Join(clauses, " "), args, needsTagJoin
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

// GetItems fetches media items from the database with pagination and search
func GetItems(db *sql.DB, offset, limit int, searchQuery string) ([]MediaItem, bool, error) {
	baseQuery := `SELECT DISTINCT m.path, m.description, m.size, m.hash FROM media m`
	var joinClause string
	orderBy := ` ORDER BY m.path`
	limitClause := ` LIMIT ? OFFSET ?`

	var query string
	var args []interface{}

	// Parse search query if provided
	sq, err := parseSearchQuery(searchQuery)
	if err != nil {
		// If parsing fails, ignore search and return all results
		log.Printf("Search query parsing failed: %v", err)
		sq = nil
	}

	// Build WHERE clause and check if we need tag joins
	whereClause, whereArgs, needsTagJoin := buildWhereClause(sq)

	// Add JOIN if needed for tag/category searches
	if needsTagJoin {
		joinClause = ` JOIN media_tag_by_category mtbc ON m.path = mtbc.media_path`
	}

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
		err := rows.Scan(&item.Path, &item.Description, &item.Size, &item.Hash)
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

	return items, hasMore, nil
}
