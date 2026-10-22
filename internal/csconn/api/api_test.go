//nolint:all // Pinned upstream copy; edits are logged in UPSTREAM.md.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestRetries(t *testing.T) {
	var callCount int
	csName := "test_codespace"
	handler := func(w http.ResponseWriter, r *http.Request) {
		if callCount == 3 {
			err := json.NewEncoder(w).Encode(Codespace{
				Name: csName,
			})
			if err != nil {
				t.Fatal(err)
			}
			return
		}
		callCount++
		w.WriteHeader(502)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, r) }))
	t.Cleanup(srv.Close)
	a := &API{
		githubAPI: srv.URL,
		client:    &http.Client{},
	}
	cs, err := a.GetCodespace(context.Background(), "test", false)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 3 {
		t.Fatalf("expected at least 2 retries but got %d", callCount)
	}
	if cs.Name != csName {
		t.Fatalf("expected codespace name to be %q but got %q", csName, cs.Name)
	}
	callCount = 0
	handler = func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		err := json.NewEncoder(w).Encode(Codespace{
			Name: csName,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	cs, err = a.GetCodespace(context.Background(), "test", false)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("expected no retries but got %d calls", callCount)
	}
	if cs.Name != csName {
		t.Fatalf("expected codespace name to be %q but got %q", csName, cs.Name)
	}
}

func TestCodespace_ExportData(t *testing.T) {
	type fields struct {
		Name        string
		CreatedAt   string
		DisplayName string
		LastUsedAt  string
		Owner       User
		Repository  Repository
		State       string
		GitStatus   CodespaceGitStatus
		Connection  CodespaceConnection
		Machine     CodespaceMachine
	}
	type args struct {
		fields []string
	}
	tests := []struct {
		name   string
		fields fields
		args   args
		want   map[string]any
	}{
		{
			name: "just name",
			fields: fields{
				Name: "test",
			},
			args: args{
				fields: []string{"name"},
			},
			want: map[string]any{
				"name": "test",
			},
		},
		{
			name: "just owner",
			fields: fields{
				Owner: User{
					Login: "test",
				},
			},
			args: args{
				fields: []string{"owner"},
			},
			want: map[string]any{
				"owner": "test",
			},
		},
		{
			name: "just machine",
			fields: fields{
				Machine: CodespaceMachine{
					Name: "test",
				},
			},
			args: args{
				fields: []string{"machineName"},
			},
			want: map[string]any{
				"machineName": "test",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Codespace{
				Name:        tt.fields.Name,
				CreatedAt:   tt.fields.CreatedAt,
				DisplayName: tt.fields.DisplayName,
				LastUsedAt:  tt.fields.LastUsedAt,
				Owner:       tt.fields.Owner,
				Repository:  tt.fields.Repository,
				State:       tt.fields.State,
				GitStatus:   tt.fields.GitStatus,
				Connection:  tt.fields.Connection,
				Machine:     tt.fields.Machine,
			}
			if got := c.ExportData(tt.args.fields); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Codespace.ExportData() = %v, want %v", got, tt.want)
			}
		})
	}
}
