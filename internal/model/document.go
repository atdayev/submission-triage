package model

import (
	"sort"
	"time"
)

// Document is a classified attachment on a submission.
type Document struct {
	ID              string
	SubmissionID    string
	EmailID         string
	Filename        string
	ContentType     string
	SizeBytes       int
	SHA256          string
	ClassifiedAs    string
	Confidence      float64
	ClassifiedBy    string
	ExtractedText   string
	ExtractedFields map[string]any
	Unreadable      bool // extraction yielded no text (e.g. a scanned PDF)
	Encrypted       bool // password-protected; the broker can resend it unlocked
	CreatedAt       time.Time
}

// Classification sources recorded in Document.ClassifiedBy.
const (
	ClassifiedByFilename        = "filename"         // filename glob only
	ClassifiedByKeyword         = "keyword"          // content keyword only
	ClassifiedByFilenameKeyword = "filename+keyword" // both agreed
	ClassifiedByLLM             = "llm"              // resolved by the model
	ClassifiedByHeuristic       = "heuristic"        // no candidate matched
)

// FilenameOnlyMatches returns the item ids a filename glob matched with no
// keyword or LLM corroboration; the digest flags these as content-unverified.
func FilenameOnlyMatches(docs []Document) []string {
	filename := map[string]bool{}
	corroborated := map[string]bool{}
	for _, d := range docs {
		if d.ClassifiedAs == "" {
			continue
		}
		switch d.ClassifiedBy {
		case ClassifiedByFilename:
			filename[d.ClassifiedAs] = true
		case ClassifiedByKeyword, ClassifiedByFilenameKeyword, ClassifiedByLLM:
			corroborated[d.ClassifiedAs] = true
		}
	}
	out := make([]string, 0, len(filename))
	for id := range filename {
		if !corroborated[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
