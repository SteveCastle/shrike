package tasks

import (
	"database/sql"
	"fmt"
	"strings"
)

// TagInfo represents a tag with its category for the auto-tagging system
type TagInfo struct {
	Label    string
	Category string
}

// ensureCategoryExists inserts the category if it doesn't already exist.
// The category table is expected to have columns: label, weight
func ensureCategoryExists(db *sql.DB, label string, weight int) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("ensureCategoryExists: empty label")
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO category (label, weight) VALUES (?, ?)`, label, weight)
	if err != nil {
		return fmt.Errorf("ensureCategoryExists: insert %s: %w", label, err)
	}
	return nil
}

// getAllAvailableTags fetches all unique tags and their categories from the database
func getAllAvailableTags(db *sql.DB) ([]TagInfo, error) {
	query := `
		SELECT DISTINCT tag_label, category_label 
		FROM media_tag_by_category 
		ORDER BY category_label, tag_label`
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query available tags: %w", err)
	}
	defer rows.Close()
	var tags []TagInfo
	for rows.Next() {
		var tag TagInfo
		if err := rows.Scan(&tag.Label, &tag.Category); err != nil {
			return nil, fmt.Errorf("failed to scan tag row: %w", err)
		}
		tags = append(tags, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating over tag rows: %w", err)
	}
	return tags, nil
}

// getExistingTagsForFile checks if a file already has tags
func getExistingTagsForFile(db *sql.DB, filePath string) ([]TagInfo, error) {
	rows, err := db.Query(`
		SELECT tag_label, category_label 
		FROM media_tag_by_category 
		WHERE media_path = ?`, filePath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []TagInfo
	for rows.Next() {
		var tag TagInfo
		if err := rows.Scan(&tag.Label, &tag.Category); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

// removeExistingTagsForFile removes all existing tags for a file
func removeExistingTagsForFile(db *sql.DB, filePath string) error {
	_, err := db.Exec(`DELETE FROM media_tag_by_category WHERE media_path = ?`, filePath)
	return err
}

// insertTagsForFile inserts tags for a file into the database
func insertTagsForFile(db *sql.DB, filePath string, tags []TagInfo) error {
	if err := ensureTagsExist(db, tags); err != nil {
		return err
	}
	stmt := `INSERT INTO media_tag_by_category (media_path, tag_label, category_label) VALUES (?, ?, ?)`
	for _, t := range tags {
		if _, err := db.Exec(stmt, filePath, t.Label, t.Category); err != nil {
			return fmt.Errorf("failed to insert tag %s/%s: %w", t.Category, t.Label, err)
		}
	}
	return nil
}

// ensureTagsExist inserts any missing tags into the tag table.
// The tag table is expected to have columns: label, category_label
func ensureTagsExist(db *sql.DB, tags []TagInfo) error {
	if len(tags) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("ensureTagsExist: begin tx: %w", err)
	}
	defer tx.Rollback()
	insertSQL := `INSERT OR IGNORE INTO tag (label, category_label) VALUES (?, ?)`
	for _, t := range tags {
		if strings.TrimSpace(t.Label) == "" {
			continue
		}
		if _, err := tx.Exec(insertSQL, t.Label, t.Category); err != nil {
			return fmt.Errorf("ensureTagsExist: insert %s/%s: %w", t.Category, t.Label, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ensureTagsExist: commit: %w", err)
	}
	return nil
}
