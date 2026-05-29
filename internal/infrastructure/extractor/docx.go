package extractor

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxDocxPartBytes = 50 << 20

type DOCX struct{}

func NewDOCX() *DOCX { return &DOCX{} }

func (e *DOCX) Extract(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx: open zip: %w", err)
	}
	var out strings.Builder
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		if f.UncompressedSize64 > maxDocxPartBytes {
			return "", fmt.Errorf("docx: part %s exceeds %d bytes", f.Name, maxDocxPartBytes)
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("docx: open part: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(rc, maxDocxPartBytes+1))
		rc.Close()
		if err != nil {
			return "", fmt.Errorf("docx: read part: %w", err)
		}
		if len(body) > maxDocxPartBytes {
			return "", fmt.Errorf("docx: part %s exceeds %d bytes after decompress", f.Name, maxDocxPartBytes)
		}
		text, err := walkDocxText(body)
		if err != nil {
			return "", err
		}
		out.WriteString(text)
	}
	return out.String(), nil
}

func walkDocxText(b []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	var out strings.Builder
	var capture bool
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docx: xml parse: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				capture = true
			}
			if t.Name.Local == "p" {
				out.WriteString("\n")
			}
		case xml.EndElement:
			if t.Name.Local == "t" {
				capture = false
			}
		case xml.CharData:
			if capture {
				out.Write(t)
			}
		}
	}
	return out.String(), nil
}
