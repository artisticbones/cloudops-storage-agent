package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Retriever is the smallest retrieval boundary used by the runtime.
type Retriever interface {
	Search(context.Context, string, string, string, int) ([]Document, error)
}

// Source identifies the durable origin of a retrieved document.
type Source struct {
	ID    string `json:"id,omitempty"`
	Title string `json:"title,omitempty"`
	URI   string `json:"uri,omitempty"`
}

// Document is a searchable text fragment. Source is retained when a document
// is chunked so an answer can cite the original material.
type Document struct {
	ID       string            `json:"id,omitempty"`
	TenantID string            `json:"tenantId,omitempty"`
	Version  string            `json:"version,omitempty"`
	Content  string            `json:"content"`
	Source   Source            `json:"source,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// MemoryRetriever provides deterministic, dependency-free keyword retrieval.
// When version is non-empty, only documents with that version are searchable.
type MemoryRetriever struct {
	documents []Document
	version   string
}

var _ Retriever = (*MemoryRetriever)(nil)

func NewMemoryRetriever(documents []Document) *MemoryRetriever {
	r := &MemoryRetriever{}
	r.Load(documents, 0)
	return r
}

func NewMemoryRetrieverWithVersion(documents []Document, version string) *MemoryRetriever {
	r := &MemoryRetriever{version: strings.TrimSpace(version)}
	r.Load(documents, 0)
	return r
}

// Load replaces the in-memory index. chunkSize is measured in runes; zero
// keeps each input document whole.
func (r *MemoryRetriever) Load(documents []Document, chunkSize int) {
	if r == nil {
		return
	}
	loaded := make([]Document, 0, len(documents))
	for _, document := range documents {
		loaded = append(loaded, ChunkDocument(document, chunkSize)...)
	}
	r.documents = loaded
}

// ChunkDocuments returns source-preserving chunks for every input document.
func ChunkDocuments(documents []Document, chunkSize int) []Document {
	chunks := make([]Document, 0, len(documents))
	for _, document := range documents {
		chunks = append(chunks, ChunkDocument(document, chunkSize)...)
	}
	return chunks
}

func ChunkDocument(document Document, chunkSize int) []Document {
	content := []rune(document.Content)
	if chunkSize <= 0 || len(content) <= chunkSize {
		return []Document{document}
	}
	chunks := make([]Document, 0, (len(content)+chunkSize-1)/chunkSize)
	for start, index := 0, 0; start < len(content); start, index = start+chunkSize, index+1 {
		end := start + chunkSize
		if end > len(content) {
			end = len(content)
		}
		chunk := document
		chunk.Content = string(content[start:end])
		chunk.ID = fmt.Sprintf("%s#%d", document.ID, index+1)
		chunks = append(chunks, chunk)
	}
	return chunks
}

func (r *MemoryRetriever) Search(ctx context.Context, tenant, version, query string, limit int) ([]Document, error) {
	tenant = strings.TrimSpace(tenant)
	version = strings.TrimSpace(version)
	if tenant == "" {
		return nil, &AgentError{Kind: InvalidArgument, Message: "retrieval tenant cannot be empty"}
	}
	if version == "" {
		return nil, &AgentError{Kind: InvalidArgument, Message: "retrieval version cannot be empty"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []Document{}, nil
	}
	if r.version != "" && r.version != version {
		return []Document{}, nil
	}
	terms := keywords(query)
	if len(terms) == 0 {
		return []Document{}, nil
	}
	type scored struct {
		document Document
		score    int
		index    int
	}
	matches := make([]scored, 0)
	for index, document := range r.documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if document.TenantID != tenant {
			continue
		}
		if document.Version != version {
			continue
		}
		text := strings.Join([]string{document.Content, document.Source.Title}, " ")
		wordSet := keywords(text)
		score := 0
		for term := range terms {
			if wordSet[term] {
				score++
			}
		}
		if score > 0 {
			matches = append(matches, scored{document: document, score: score, index: index})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].index < matches[j].index
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	result := make([]Document, len(matches))
	for i, match := range matches {
		result[i] = match.document
	}
	return result, nil
}

func keywords(text string) map[string]bool {
	result := make(map[string]bool)
	var word []rune
	flush := func() {
		if len(word) > 0 {
			result[string(word)] = true
			word = word[:0]
		}
	}
	for _, char := range strings.ToLower(text) {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			word = append(word, char)
			continue
		}
		flush()
	}
	flush()
	return result
}

func formatRetrievedDocuments(documents []Document) string {
	if len(documents) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("Retrieved context (cite the source IDs/URIs in the answer):\n")
	for _, document := range documents {
		builder.WriteString("- [")
		if document.Source.ID != "" {
			builder.WriteString(document.Source.ID)
		} else {
			builder.WriteString(document.ID)
		}
		if document.ID != "" && document.ID != document.Source.ID {
			builder.WriteString(" doc=")
			builder.WriteString(document.ID)
		}
		builder.WriteString("]")
		if document.Source.Title != "" {
			builder.WriteString(" ")
			builder.WriteString(document.Source.Title)
		}
		if document.Source.URI != "" {
			builder.WriteString(" <")
			builder.WriteString(document.Source.URI)
			builder.WriteString(">")
		}
		builder.WriteString(": ")
		builder.WriteString(document.Content)
		builder.WriteByte('\n')
	}
	return strings.TrimRight(builder.String(), "\n")
}
