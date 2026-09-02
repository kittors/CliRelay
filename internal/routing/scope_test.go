package routing

import "testing"

func TestNormalizeNamespacePathAcceptsFullURL(t *testing.T) {
	t.Parallel()

	if got := NormalizeNamespacePath("https://relay.example.com/pro"); got != "/pro" {
		t.Fatalf("NormalizeNamespacePath(full URL) = %q, want %q", got, "/pro")
	}
}

func TestNormalizeNamespacePathAcceptsMultiSegmentPath(t *testing.T) {
	t.Parallel()

	if got := NormalizeNamespacePath("/openai/pro"); got != "/openai/pro" {
		t.Fatalf("NormalizeNamespacePath(multi segment) = %q, want %q", got, "/openai/pro")
	}
}

func TestNormalizeNamespacePathAcceptsMultiSegmentFullURL(t *testing.T) {
	t.Parallel()

	if got := NormalizeNamespacePath("https://relay.example.com/openai/pro"); got != "/openai/pro" {
		t.Fatalf("NormalizeNamespacePath(multi segment URL) = %q, want %q", got, "/openai/pro")
	}
}

func TestNormalizeNamespacePathAcceptsFullURLWithUnicodePath(t *testing.T) {
	t.Parallel()

	if got := NormalizeNamespacePath("https://relay.example.com/%E8%B7%AF%E5%BE%84"); got != "/路径" {
		t.Fatalf("NormalizeNamespacePath(encoded URL) = %q, want %q", got, "/路径")
	}
}
