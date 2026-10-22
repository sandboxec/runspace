//nolint:depguard // No depguard allow-list exists without a config file; these imports are required.
package main

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.sandbox.ec/runspace/internal/mcpserver"
	"go.sandbox.ec/runspace/internal/sandbox"
	"go.sandbox.ec/runspace/internal/sandbox/sandboxtest"
)

// mcpOnlyFields is the MCP half of the transport list.
func mcpOnlyFields() map[string][]string {
	return map[string][]string{
		cmdExec: {"stdin"},
		cmdRun:  {"stdin"},
	}
}

// listMCPTools serves the runspace tools in memory and returns what a client
// sees from tools/list.
func listMCPTools(t *testing.T) []*mcp.Tool {
	t.Helper()

	sandboxes, err := sandbox.New(sandbox.Config{
		Token:     "",
		Getenv:    func(string) string { return "" },
		APIClient: new(sandboxtest.GitHub).Client(),
		Dial:      nil,
		Logger:    nil,
		Reuse:     false,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = sandboxes.Close() })

	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	serverSession, err := mcpserver.New(sandboxes, nil, "test").Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{
		Name: "parity", Title: "", Description: "", Version: "", WebsiteURL: "", Icons: nil,
	}, nil)

	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = clientSession.Close() })

	list, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	return list.Tools
}

// Every tool's input schema has exactly the options fields plus the
// transport list, with the same required fields.
func TestMCPParity(t *testing.T) {
	t.Parallel()

	var tools []string

	for _, tool := range listMCPTools(t) {
		tools = append(tools, tool.Name)

		typ, ok := commandOptions()[tool.Name]
		if !ok {
			t.Errorf("tool %s has no command", tool.Name)

			continue
		}

		checkToolSchema(t, tool, typ)
	}

	slices.Sort(tools)

	if want := commandNames(); !slices.Equal(tools, want) {
		t.Errorf("tools = %v, want %v", tools, want)
	}
}

// checkToolSchema checks one tool's input schema against the options struct
// of its command.
func checkToolSchema(t *testing.T, tool *mcp.Tool, typ reflect.Type) {
	t.Helper()

	schema, _ := tool.InputSchema.(map[string]any)
	props, _ := schema["properties"].(map[string]any)

	wantProps := slices.Clone(mcpOnlyFields()[tool.Name])

	var wantRequired []string

	for _, opt := range optionFields(typ) {
		wantProps = append(wantProps, opt.json)

		if opt.required {
			wantRequired = append(wantRequired, opt.json)
		}
	}

	slices.Sort(wantProps)
	slices.Sort(wantRequired)

	gotProps := slices.Sorted(maps.Keys(props))
	gotRequired := requiredFields(t, schema)

	if !slices.Equal(gotProps, wantProps) {
		t.Errorf("%s: properties %v, want %v", tool.Name, gotProps, wantProps)
	}

	if !slices.Equal(gotRequired, wantRequired) {
		t.Errorf("%s: required %v, want %v", tool.Name, gotRequired, wantRequired)
	}

	if schema["additionalProperties"] != false {
		t.Errorf("%s: additional properties allowed", tool.Name)
	}
}

// requiredFields returns the sorted names in a JSON schema's required list.
func requiredFields(t *testing.T, schema map[string]any) []string {
	t.Helper()

	list, _ := schema["required"].([]any)
	required := make([]string, 0, len(list))

	for _, item := range list {
		name, ok := item.(string)
		if !ok {
			t.Fatalf("required entry %v is not a string", item)
		}

		required = append(required, name)
	}

	slices.Sort(required)

	return required
}
