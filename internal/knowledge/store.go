package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/store"
)

// SyncStore rebuilds SQLite knowledge metadata from canonical Markdown.
func (r *Repository) SyncStore(target *store.Store, sourceRoot string, now time.Time) error {
	docs, err := r.List()
	if err != nil {
		return err
	}
	rows := make([]store.KnowledgeDocument, 0, len(docs))
	var anchors []store.SourceAnchor
	for _, doc := range docs {
		encoded, err := r.Codec.Marshal(doc)
		if err != nil {
			return err
		}
		conceptID := doc.Prowl.ID
		if conceptID == "" {
			conceptID = strings.TrimSuffix(filepath.ToSlash(doc.Path), filepath.Ext(doc.Path))
		}
		knowledgeID := stableID("concept", conceptID)
		tags, _ := json.Marshal(doc.Tags)
		metadata, _ := json.Marshal(doc.Prowl)
		contentSum := sha256.Sum256(encoded)
		state := doc.Prowl.Status
		if state == "" {
			state = "confirmed"
		}
		rows = append(rows, store.KnowledgeDocument{
			ID: knowledgeID, ConceptID: conceptID, Path: doc.Path, Type: doc.Type,
			Title: doc.Title, Description: doc.Description, Resource: doc.Resource,
			TagsJSON: string(tags), Timestamp: doc.Timestamp, ReviewState: state,
			ContentHash: "sha256:" + hex.EncodeToString(contentSum[:]), MetadataJSON: string(metadata),
		})
		for index, anchor := range doc.Prowl.Anchors {
			check := CheckAnchor(sourceRoot, anchor)
			anchorMetadata, _ := json.Marshal(anchor)
			// A moved anchor is indexed at the lines its content occupies now, so
			// retrieval cites a location that resolves; the document keeps its
			// authored range until a repair rewrites it.
			lineStart, lineEnd := check.Region()
			anchors = append(anchors, store.SourceAnchor{
				ID:          stableID("anchor", conceptID, strconv.Itoa(index), anchor.Path, anchor.ContentHash),
				KnowledgeID: knowledgeID, URI: "file://" + filepath.ToSlash(anchor.Path),
				LineStart: lineStart, LineEnd: lineEnd,
				ContentHash: anchor.ContentHash, RegionHash: check.Actual,
				Status: string(check.Status), CheckedAt: now.UTC().Format(time.RFC3339),
				MetadataJSON: string(anchorMetadata),
			})
		}
	}
	return target.ReplaceKnowledge(rows, anchors)
}

func stableID(namespace string, parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte{0})
		hash.Write([]byte(part))
	}
	return namespace + ":" + hex.EncodeToString(hash.Sum(nil))[:24]
}
