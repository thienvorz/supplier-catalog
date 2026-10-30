package supplier

import (
	"encoding/json"
	"fmt"
	"os"
)

// FeedFile is the on-disk format of the fictional supplier's data files in
// data/feeds/. Each element of Batches is one batch of raw records.
type FeedFile struct {
	Supplier    string              `json:"supplier"`
	Description string              `json:"description"`
	Batches     [][]json.RawMessage `json:"batches"`
}

// ReadFeedFile loads a feed file from disk.
func ReadFeedFile(path string) (FeedFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FeedFile{}, err
	}
	var f FeedFile
	if err := json.Unmarshal(data, &f); err != nil {
		return FeedFile{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}
