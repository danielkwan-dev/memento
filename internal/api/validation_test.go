package api

import "testing"

// Username validation runs before anything reaches the scraper, so junk never
// becomes a background job or a request to Letterboxd.
func TestUsernameValidation(t *testing.T) {
	valid := []string{
		"dave",
		"ab",                               // the 2-char minimum
		"a_b",                              // underscores are allowed
		"User123",                          // mixed case (lowercased later)
		"abcdefghijklmnopqrstuvwxyz123456", // exactly 32 chars
	}
	for _, u := range valid {
		if !usernameRe.MatchString(u) {
			t.Errorf("username %q should be valid", u)
		}
	}

	invalid := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"single char", "a"},
		{"33 chars", "abcdefghijklmnopqrstuvwxyz1234567"},
		{"path traversal", "../admin"},
		{"slash", "dave/films"},
		{"space", "dave smith"},
		{"hyphen", "dave-smith"},
		{"dot", "dave.smith"},
		{"at sign", "dave@example.com"},
		{"url", "https://letterboxd.com/dave/"},
		{"percent encoding", "dave%2f"},
		{"newline", "dave\n"},
		{"sql-ish", "dave' OR '1'='1"},
	}
	for _, tc := range invalid {
		if usernameRe.MatchString(tc.input) {
			t.Errorf("%s: username %q should be rejected", tc.name, tc.input)
		}
	}
}
