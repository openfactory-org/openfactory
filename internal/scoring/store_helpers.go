package scoring

import (
	"context"
	"sort"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

const pageSize = 500

// listAllRuns pages through ListRuns (ordered created DESC, id DESC).
func listAllRuns(ctx context.Context, st core.Store, f core.RunFilter) ([]core.Run, error) {
	f.Limit = pageSize
	f.Cursor = ""
	var out []core.Run
	seen := map[string]bool{}
	for {
		page, err := st.ListRuns(ctx, f)
		if err != nil {
			return nil, err
		}
		added := 0
		for _, r := range page {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
				added++
			}
		}
		if len(page) < pageSize || added == 0 {
			return out, nil
		}
		last := page[len(page)-1]
		f.Cursor = storeutil.EncodeCursor(last.CreatedAt, last.ID)
	}
}

func sortRunsByCreated(runs []core.Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		return runs[i].ID < runs[j].ID
	})
}

// descendants returns all descendant runs of rootID (breadth-first, bounded).
func descendants(ctx context.Context, st core.Store, rootID string) ([]core.Run, error) {
	var out []core.Run
	queue := []string{rootID}
	seen := map[string]bool{rootID: true}
	for len(queue) > 0 && len(out) < maxDescendants {
		id := queue[0]
		queue = queue[1:]
		children, err := listAllRuns(ctx, st, core.RunFilter{ParentRunID: id})
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			out = append(out, c)
			queue = append(queue, c.ID)
		}
	}
	return out, nil
}
