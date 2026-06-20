package management

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSplitAuthFileArray(t *testing.T) {
	cases := []struct {
		name      string
		data      string
		wantArray bool
		wantLen   int
	}{
		{name: "object", data: `{"type":"codex"}`, wantArray: false},
		{name: "array two", data: `[{"a":1},{"b":2}]`, wantArray: true, wantLen: 2},
		{name: "empty array", data: `[]`, wantArray: true, wantLen: 0},
		{name: "leading whitespace array", data: "  \n\t[{\"a\":1}]", wantArray: true, wantLen: 1},
		{name: "invalid array json", data: `[{"a":1},`, wantArray: false},
		{name: "empty data", data: ``, wantArray: false},
		{name: "json string", data: `"[1,2]"`, wantArray: false},
		{name: "array of scalars", data: `[1,2,3]`, wantArray: true, wantLen: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			elements, isArray := splitAuthFileArray([]byte(tc.data))
			if isArray != tc.wantArray {
				t.Fatalf("isArray = %v, want %v", isArray, tc.wantArray)
			}
			if isArray && len(elements) != tc.wantLen {
				t.Fatalf("len(elements) = %d, want %d", len(elements), tc.wantLen)
			}
		})
	}
}

func TestSplitAuthFileArray_PreservesElementBytes(t *testing.T) {
	elements, isArray := splitAuthFileArray([]byte(`[ {"type":"codex","email":"a@b.com"} ]`))
	if !isArray || len(elements) != 1 {
		t.Fatalf("expected single element array, got isArray=%v len=%d", isArray, len(elements))
	}
	if string(elements[0]) != `{"type":"codex","email":"a@b.com"}` {
		t.Fatalf("element bytes not preserved: %q", string(elements[0]))
	}
}

func TestAuthFileNameForArrayElement(t *testing.T) {
	h := &Handler{}
	cases := []struct {
		name     string
		baseName string
		index    int
		data     string
		want     string
	}{
		{name: "type and email", baseName: "cpa.json", index: 0, data: `{"type":"codex","email":"a@example.com"}`, want: "codex-a@example.com.json"},
		{name: "email no type", baseName: "cpa.json", index: 0, data: `{"email":"a@example.com"}`, want: "a@example.com.json"},
		{name: "account_id fallback", baseName: "cpa.json", index: 0, data: `{"type":"codex","account_id":"acc-1"}`, want: "codex-acc-1.json"},
		{name: "email preferred over account_id", baseName: "cpa.json", index: 0, data: `{"type":"codex","email":"a@example.com","account_id":"acc-1"}`, want: "codex-a@example.com.json"},
		{name: "whitespace email trimmed", baseName: "cpa.json", index: 0, data: `{"type":"codex","email":"  a@example.com  "}`, want: "codex-a@example.com.json"},
		{name: "no identifier index fallback", baseName: "cpa.json", index: 2, data: `{"type":"codex"}`, want: "cpa-3.json"},
		{name: "invalid element index fallback", baseName: "cpa.json", index: 0, data: `12345`, want: "cpa-1.json"},
		{name: "empty base name fallback", baseName: "", index: 0, data: `{"type":"codex"}`, want: "auth-1.json"},
		{name: "slash in identifier sanitized", baseName: "cpa.json", index: 0, data: `{"type":"codex","email":"a/b@x"}`, want: "b@x.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := h.authFileNameForArrayElement(tc.baseName, tc.index, []byte(tc.data))
			if got != tc.want {
				t.Fatalf("authFileNameForArrayElement = %q, want %q", got, tc.want)
			}
		})
	}
}

func newArrayTestHandler(t *testing.T) (*Handler, *coreauth.Manager, string) {
	t.Helper()
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	return h, manager, authDir
}

func TestWriteAuthFile_EmptyArrayReturnsError(t *testing.T) {
	h, _, _ := newArrayTestHandler(t)
	if err := h.writeAuthFile(context.Background(), "cpa.json", []byte(`[]`)); err == nil {
		t.Fatal("expected error for empty array, got nil")
	}
}

func TestWriteAuthFile_PartialSuccessWritesValidEntries(t *testing.T) {
	h, manager, authDir := newArrayTestHandler(t)

	// Second element (a scalar) cannot be parsed as an auth object.
	body := `[{"type":"codex","email":"ok@example.com"},12345]`
	if err := h.writeAuthFile(context.Background(), "cpa.json", []byte(body)); err != nil {
		t.Fatalf("expected best-effort success, got error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(authDir, "codex-ok@example.com.json")); err != nil {
		t.Fatalf("expected valid entry to be written: %v", err)
	}
	// The invalid entry must not leave a file behind.
	if _, err := os.Stat(filepath.Join(authDir, "cpa-2.json")); !os.IsNotExist(err) {
		t.Fatalf("expected no file for invalid entry, stat err = %v", err)
	}
	if auths := manager.List(); len(auths) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(auths))
	}
}

func TestWriteAuthFile_AllEntriesInvalidReturnsError(t *testing.T) {
	h, manager, _ := newArrayTestHandler(t)
	if err := h.writeAuthFile(context.Background(), "cpa.json", []byte(`[1,2]`)); err == nil {
		t.Fatal("expected error when all entries are invalid, got nil")
	}
	if auths := manager.List(); len(auths) != 0 {
		t.Fatalf("expected 0 auth entries, got %d", len(auths))
	}
}

func TestWriteAuthFile_DuplicateEmailLastWins(t *testing.T) {
	h, manager, authDir := newArrayTestHandler(t)

	body := `[{"type":"codex","email":"dup@example.com","access_token":"first"},{"type":"codex","email":"dup@example.com","access_token":"second"}]`
	if err := h.writeAuthFile(context.Background(), "cpa.json", []byte(body)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(authDir, "codex-dup@example.com.json"))
	if err != nil {
		t.Fatalf("expected merged file to exist: %v", err)
	}
	if want := `"access_token":"second"`; !strings.Contains(string(data), want) {
		t.Fatalf("expected last entry to win, file = %s", string(data))
	}
	if auths := manager.List(); len(auths) != 1 {
		t.Fatalf("expected 1 auth entry after dedup, got %d", len(auths))
	}
}

func TestWriteAuthFile_NonArrayStillWritesSingleFile(t *testing.T) {
	h, manager, authDir := newArrayTestHandler(t)

	if err := h.writeAuthFile(context.Background(), "single.json", []byte(`{"type":"codex","email":"x@example.com"}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "single.json")); err != nil {
		t.Fatalf("expected single file to be written verbatim: %v", err)
	}
	if auths := manager.List(); len(auths) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(auths))
	}
}
