package main

// lineChange represents a single change in the screen output diff.
// Operations: "replace" (line + text), "append" (text only), "delete" (line + count).
type lineChange struct {
	Op    string `json:"op"`
	Line  int    `json:"line"`
	Text  string `json:"text,omitempty"`
	Count int    `json:"count,omitempty"`
}

// diffLines computes a minimal set of line changes to transform prev into curr.
// Returns nil if the two slices are identical.
func diffLines(prev, curr []string) []lineChange {
	// Fast path: identical content.
	if len(prev) == len(curr) {
		same := true
		for i := range prev {
			if prev[i] != curr[i] {
				same = false
				break
			}
		}
		if same {
			return nil
		}
	}

	var changes []lineChange

	// Emit replacements for lines that differ within the shared range.
	shared := len(prev)
	if len(curr) < shared {
		shared = len(curr)
	}
	for i := 0; i < shared; i++ {
		if prev[i] != curr[i] {
			changes = append(changes, lineChange{
				Op:   "replace",
				Line: i,
				Text: curr[i],
			})
		}
	}

	// If curr is longer, append the new lines.
	if len(curr) > len(prev) {
		for i := len(prev); i < len(curr); i++ {
			changes = append(changes, lineChange{
				Op:   "append",
				Text: curr[i],
			})
		}
	}

	// If curr is shorter, delete the trailing lines.
	if len(curr) < len(prev) {
		changes = append(changes, lineChange{
			Op:    "delete",
			Line:  len(curr),
			Count: len(prev) - len(curr),
		})
	}

	return changes
}
