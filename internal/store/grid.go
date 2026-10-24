package store

import (
	"context"
	"strings"
)

// GridSort names a grid ordering. Unknown values fall back to SortAdded.
type GridSort string

// Supported grid orderings.
const (
	SortAdded    GridSort = "added"
	SortName     GridSort = "name"
	SortViews    GridSort = "views"
	SortRating   GridSort = "rating"
	SortDuration GridSort = "duration"
	SortRecent   GridSort = "recent" // recently watched by the user
)

var gridOrder = map[GridSort]string{
	SortAdded:    "m.added_at DESC, m.id DESC",
	SortName:     "m.title COLLATE NOCASE ASC, m.id",
	SortViews:    "views DESC, m.title COLLATE NOCASE",
	SortRating:   "stars DESC, m.title COLLATE NOCASE",
	SortDuration: "m.duration_ms DESC, m.id",
	SortRecent:   "pb.updated_at DESC NULLS LAST, m.id DESC",
}

// GridQuery selects one page of the grid.
type GridQuery struct {
	UserID int64
	// Root and Dir restrict the result to one library folder and its
	// subfolders. An empty Root means the whole library.
	Root, Dir string
	Search    string
	Sort      GridSort
	// ContinueOnly selects only partially watched videos.
	ContinueOnly bool
	Limit        int
	Offset       int
}

// GridItem is one card in the grid.
type GridItem struct {
	ID          int64
	PID         string
	Title       string
	DurationMs  int64
	Width       int64
	Height      int64
	HasThumb    bool
	Playability string
	PositionMs  int64
	Completed   bool
	Stars       int64
	Views       int64
}

// ValidSort reports whether s names a supported ordering.
func ValidSort(s string) bool {
	_, ok := gridOrder[GridSort(s)]
	return ok
}

// Grid returns one page of videos. It is hand-written rather than generated
// because the ordering and filters are dynamic.
func (s *Store) Grid(ctx context.Context, gq GridQuery) ([]GridItem, error) {
	var (
		where = []string{"m.status = 'ok'"}
		args  = []any{gq.UserID, gq.UserID}
	)
	if gq.Root != "" {
		where = append(where, "m.root = ?")
		args = append(args, gq.Root)
		if gq.Dir != "" {
			where = append(where, `(m.rel_dir = ? OR m.rel_dir LIKE ? ESCAPE '\')`)
			args = append(args, gq.Dir, escapeLike(gq.Dir)+"/%")
		}
	}
	if q := strings.TrimSpace(gq.Search); q != "" {
		where = append(where, `m.title LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(q)+"%")
	}
	if gq.ContinueOnly {
		where = append(where, "pb.position_ms > 0 AND pb.completed = 0")
	}
	order, ok := gridOrder[gq.Sort]
	if !ok {
		order = gridOrder[SortAdded]
	}
	if gq.Limit <= 0 {
		gq.Limit = 48
	}
	args = append(args, gq.Limit, gq.Offset)

	//nolint:gosec // G202: only fixed WHERE fragments and whitelisted ORDER BY clauses are concatenated.
	query := `
SELECT m.id, m.pid, m.title, m.duration_ms, m.width, m.height, m.has_thumb, m.playability,
    COALESCE(pb.position_ms, 0), COALESCE(pb.completed, 0), COALESCE(r.stars, 0) AS stars,
    (SELECT COUNT(*) FROM play_events e WHERE e.media_id = m.id AND e.counted = 1) AS views
FROM media m
LEFT JOIN playback pb ON pb.media_id = m.id AND pb.user_id = ?
LEFT JOIN ratings r ON r.media_id = m.id AND r.user_id = ?
WHERE ` + strings.Join(where, " AND ") + `
ORDER BY ` + order + `
LIMIT ? OFFSET ?`

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var items []GridItem
	for rows.Next() {
		var (
			it                  GridItem
			hasThumb, completed int64
		)
		if err := rows.Scan(&it.ID, &it.PID, &it.Title, &it.DurationMs, &it.Width, &it.Height,
			&hasThumb, &it.Playability, &it.PositionMs, &completed, &it.Stars, &it.Views); err != nil {
			return nil, err
		}
		it.HasThumb = hasThumb != 0
		it.Completed = completed != 0
		items = append(items, it)
	}
	return items, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
