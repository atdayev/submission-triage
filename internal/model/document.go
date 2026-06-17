package model

import "time"

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
	CreatedAt       time.Time
}
