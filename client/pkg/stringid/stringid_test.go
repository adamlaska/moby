package stringid

import "testing"

func TestGenerateRandomID(t *testing.T) {
	id := GenerateRandomID()

	if len(id) != fullLen {
		t.Fatalf("Id returned is incorrect: %s", id)
	}
}

func TestTruncateID(t *testing.T) {
	tests := []struct {
		doc, id, expected string
	}{
		{
			doc:      "empty ID",
			id:       "",
			expected: "",
		},
		{
			// IDs are expected to be 12 (short) or 64 characters, and not be numeric only,
			// but TruncateID should handle these gracefully.
			doc:      "invalid ID",
			id:       "1234",
			expected: "1234",
		},
		{
			doc:      "full ID",
			id:       "90435eec5c4e124e741ef731e118be2fc799a68aba0466ec17717f24ce2ae6a2",
			expected: "90435eec5c4e",
		},
		{
			doc:      "digest",
			id:       "sha256:90435eec5c4e124e741ef731e118be2fc799a68aba0466ec17717f24ce2ae6a2",
			expected: "90435eec5c4e",
		},
		{
			doc:      "very long ID",
			id:       "90435eec5c4e124e741ef731e118be2fc799a68aba0466ec17717f24ce2ae6a290435eec5c4e124e741ef731e118be2fc799a68aba0466ec17717f24ce2ae6a2",
			expected: "90435eec5c4e",
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			actual := TruncateID(tc.id)
			if actual != tc.expected {
				t.Errorf("expected: %q, got: %q", tc.expected, actual)
			}
		})
	}
}
