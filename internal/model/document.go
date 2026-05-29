package model

import "time"

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
