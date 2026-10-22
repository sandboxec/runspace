package mcpserver_test

import (
	"context"
	"regexp"
	"testing"
)

// Clients get one line for each paragraph and list item of a tool
// description, however the source is wrapped.
func TestToolDescriptionsUnwrapped(t *testing.T) {
	t.Parallel()

	fix := newFixture(t, nil)

	list, err := fix.client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// A line break that neither ends a paragraph nor starts a list item.
	inParagraph := regexp.MustCompile(`[^\n]\n[^\n-]`)

	for _, tool := range list.Tools {
		if found := inParagraph.FindString(tool.Description); found != "" {
			t.Errorf("%s description breaks a line inside a paragraph at %q", tool.Name, found)
		}
	}

	if len(list.Tools) != 7 {
		t.Errorf("listed %d tools, want 7", len(list.Tools))
	}
}
