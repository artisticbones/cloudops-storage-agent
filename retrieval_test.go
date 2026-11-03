package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMemoryRetrieverFiltersTenantAndVersionAndReturnsSource(t *testing.T) {
	r := NewMemoryRetrieverWithVersion([]Document{
		{ID: "a", TenantID: "tenant-a", Version: "v1", Content: "refund policy for annual plans", Source: Source{ID: "s-a", Title: "Billing", URI: "kb://billing"}},
		{ID: "b", TenantID: "tenant-a", Version: "v2", Content: "refund policy for monthly plans", Source: Source{ID: "s-b", Title: "Billing v2", URI: "kb://billing-v2"}},
		{ID: "c", TenantID: "tenant-b", Version: "v1", Content: "refund policy for another tenant", Source: Source{ID: "s-c", URI: "kb://other"}},
	}, "v1")

	docs, err := r.Search(context.Background(), "tenant-a", "v1", "refund policy", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "a" || docs[0].Source.URI != "kb://billing" {
		t.Fatalf("Search() = %#v, want only tenant-a/v1 document with source", docs)
	}
}

func TestMemoryRetrieverChunksLoadedDocuments(t *testing.T) {
	r := NewMemoryRetriever(nil)
	r.Load([]Document{{ID: "doc", TenantID: "t", Version: "v1", Content: "one two three four five", Source: Source{URI: "kb://doc"}}}, 15)

	docs, err := r.Search(context.Background(), "t", "v1", "three", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(docs) != 1 || !strings.Contains(docs[0].Content, "three") || docs[0].Source.URI != "kb://doc" {
		t.Fatalf("chunked Search() = %#v, want source-preserving matching chunk", docs)
	}
}

func TestMemoryRetrieverReturnsEmptyRecall(t *testing.T) {
	r := NewMemoryRetriever([]Document{{TenantID: "t", Version: "v1", Content: "alpha"}})
	docs, err := r.Search(context.Background(), "t", "v1", "missing", 5)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("Search() = %#v, want empty recall", docs)
	}
}

func TestMemoryRetrieverHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewMemoryRetriever(nil).Search(ctx, "t", "v1", "query", 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search() error = %v, want context.Canceled", err)
	}
}

type fakeRetriever struct {
	documents []Document
	err       error
	calls     int
}

func (r *fakeRetriever) Search(ctx context.Context, tenant, version, query string, limit int) ([]Document, error) {
	r.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.documents, nil
}

func TestMemoryRetrieverRejectsEmptyTenantAndVersion(t *testing.T) {
	r := NewMemoryRetriever([]Document{{TenantID: "tenant-a", Version: "v1", Content: "secret"}})
	for name, scope := range map[string][2]string{
		"empty tenant":  [2]string{"", "v1"},
		"empty version": [2]string{"tenant-a", ""},
	} {
		t.Run(name, func(t *testing.T) {
			tenant, version := scope[0], scope[1]
			_, err := r.Search(context.Background(), tenant, version, "secret", 10)
			var agentErr *AgentError
			if !errors.As(err, &agentErr) || agentErr.Kind != InvalidArgument {
				t.Fatalf("Search() error = %v, want invalid argument", err)
			}
		})
	}
}

func TestMemoryRetrieverSortsByRelevanceThenInputOrder(t *testing.T) {
	r := NewMemoryRetriever([]Document{
		{ID: "one", TenantID: "t", Version: "v1", Content: "refund policy"},
		{ID: "two", TenantID: "t", Version: "v1", Content: "refund policy annual plans"},
		{ID: "three", TenantID: "t", Version: "v1", Content: "refund"},
	})
	docs, err := r.Search(context.Background(), "t", "v1", "refund policy annual", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := []string{docs[0].ID, docs[1].ID, docs[2].ID}; !equalStrings(got, []string{"two", "one", "three"}) {
		t.Fatalf("document order = %v, want relevance order", got)
	}
}

func TestFormatRetrievedDocumentsOmitsMetadataButKeepsSourceIdentifiers(t *testing.T) {
	content := formatRetrievedDocuments([]Document{{
		ID: "doc-1", Content: "public answer", Metadata: map[string]string{"secret": "do-not-send"},
		Source: Source{ID: "source-1", URI: "kb://public"},
	}})
	if strings.Contains(content, "do-not-send") || strings.Contains(content, "secret") {
		t.Fatalf("retrieval context leaked metadata: %q", content)
	}
	if !strings.Contains(content, "source-1") || !strings.Contains(content, "kb://public") {
		t.Fatalf("retrieval context lost source identifiers: %q", content)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestContextBuilderInjectsRetrievalAndSourceCitation(t *testing.T) {
	b := NewContextBuilderWithRetriever(0, &fakeRetriever{err: errors.New("unused")})
	request, _, err := b.Build(context.Background(), RuntimeState{
		Messages:           []Message{{Role: "user", Content: "what is the policy?"}},
		RetrievedDocuments: []Document{{ID: "doc-1", Content: "Refunds are allowed within 30 days.", Source: Source{ID: "src-1", Title: "Refunds", URI: "kb://refunds"}}},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(request.Messages) != 2 || !strings.Contains(request.Messages[0].Content, "kb://refunds") || !strings.Contains(request.Messages[0].Content, "doc-1") {
		t.Fatalf("request messages = %#v, want source-aware retrieval context", request.Messages)
	}
}

func TestContextBuilderPropagatesRetrieverErrorAndCancellation(t *testing.T) {
	wantErr := errors.New("index unavailable")
	b := NewContextBuilder(0)
	_, _, err := b.Build(context.Background(), RuntimeState{
		Messages: []Message{{Role: "user", Content: "query"}}, Retriever: &fakeRetriever{err: wantErr},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Build() error = %v, want retriever error", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = b.Build(ctx, RuntimeState{
		Messages: []Message{{Role: "user", Content: "query"}}, Retriever: NewMemoryRetriever(nil), RetrievalTenant: "t",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Build() canceled error = %v, want context.Canceled", err)
	}
}
