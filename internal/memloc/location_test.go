package memloc

import (
	"os"
	"path/filepath"
	"testing"

	"reverie/internal/store"
)

func TestLocalDirFor(t *testing.T) {
	if got := LocalDirFor(""); got != "" {
		t.Fatalf("LocalDirFor(\"\") = %q, want empty", got)
	}
	want := filepath.Join("repo", ".claude", "engram")
	if got := LocalDirFor("repo"); got != want {
		t.Fatalf("LocalDirFor = %q, want %q", got, want)
	}
}

// TestHasLocal_RequiresExplicitOptIn — the directory is the opt-in signal, so a
// plain repository must not report local memory.
func TestHasLocal_RequiresExplicitOptIn(t *testing.T) {
	root := t.TempDir()
	if HasLocal(root) {
		t.Fatal("fresh directory should not have local memory")
	}
	if _, err := InitLocal(root); err != nil {
		t.Fatalf("InitLocal: %v", err)
	}
	if !HasLocal(root) {
		t.Fatal("after InitLocal, HasLocal should be true")
	}
}

// TestInitLocal_Idempotent — re-running init must not fail or reset anything.
func TestInitLocal_Idempotent(t *testing.T) {
	root := t.TempDir()
	first, err := InitLocal(root)
	if err != nil {
		t.Fatalf("first InitLocal: %v", err)
	}
	second, err := InitLocal(root)
	if err != nil {
		t.Fatalf("second InitLocal: %v", err)
	}
	if first != second {
		t.Fatalf("InitLocal returned %q then %q", first, second)
	}
	if _, err := os.Stat(filepath.Join(second, "engram.db")); err != nil {
		t.Fatalf("engram.db missing: %v", err)
	}
}

// TestOpen_NoLocalStoreLeavesGlobalOnly — the critical regression guarantee: a
// repository that never opted in behaves exactly as before.
func TestOpen_NoLocalStoreLeavesGlobalOnly(t *testing.T) {
	set := Open(nil, t.TempDir())
	defer set.Close()

	if set.HasLocalStore() {
		t.Fatal("expected no local store")
	}
	if set.DefaultWrite() != Global {
		t.Fatalf("DefaultWrite = %q, want global", set.DefaultWrite())
	}
	if set.Warning != "" {
		t.Fatalf("unexpected warning: %q", set.Warning)
	}
	if _, err := set.Store(Local); err == nil {
		t.Fatal("Store(Local) should fail without a local store")
	}
}

// TestOpen_DoesNotCreateTheDirectory guards the subtle failure mode: store.New
// does MkdirAll, so probing the store before checking opt-in would silently
// enroll every repository.
func TestOpen_DoesNotCreateTheDirectory(t *testing.T) {
	root := t.TempDir()
	set := Open(nil, root)
	defer set.Close()

	if _, err := os.Stat(LocalDirFor(root)); !os.IsNotExist(err) {
		t.Fatalf("Open created the local directory; stat err = %v", err)
	}
}

func TestOpen_ReadsDefaultWriteFromConfig(t *testing.T) {
	root := t.TempDir()
	dir, err := InitLocal(root)
	if err != nil {
		t.Fatalf("InitLocal: %v", err)
	}

	set := Open(nil, root)
	if set.DefaultWrite() != Global {
		t.Fatalf("default DefaultWrite = %q, want global", set.DefaultWrite())
	}
	set.Close()

	if err := WriteDefaultWrite(dir, Local); err != nil {
		t.Fatalf("WriteDefaultWrite: %v", err)
	}
	set2 := Open(nil, root)
	defer set2.Close()
	if set2.DefaultWrite() != Local {
		t.Fatalf("configured DefaultWrite = %q, want local", set2.DefaultWrite())
	}
}

// TestOpen_MalformedConfigFallsBackToGlobal — a broken config must not change
// where memories go.
func TestOpen_MalformedConfigFallsBackToGlobal(t *testing.T) {
	root := t.TempDir()
	dir, err := InitLocal(root)
	if err != nil {
		t.Fatalf("InitLocal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, configName), []byte("{not json"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	set := Open(nil, root)
	defer set.Close()
	if set.DefaultWrite() != Global {
		t.Fatalf("DefaultWrite = %q, want global", set.DefaultWrite())
	}
}

func TestWriteDefaultWrite_RejectsBoth(t *testing.T) {
	root := t.TempDir()
	dir, err := InitLocal(root)
	if err != nil {
		t.Fatalf("InitLocal: %v", err)
	}
	if err := WriteDefaultWrite(dir, Both); err == nil {
		t.Fatal("WriteDefaultWrite(Both) should fail")
	}
}

// TestNormalize covers the whole resolution matrix. The row that matters most is
// explicit "local" with no local store: it must be an error, never a silent
// redirect to the shared store.
func TestNormalize(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		hasLocal bool
		def      Location
		want     Location
		wantErr  bool
	}{
		{"empty read default merges", "", true, Both, Both, false},
		// Without a local store there is nothing to merge, so the read default
		// collapses to global rather than claiming to span two stores.
		{"empty read default without local", "", false, Both, Global, false},
		{"empty write default global", "", true, Global, Global, false},
		{"empty write default local", "", true, Local, Local, false},
		{"empty write default local without store", "", false, Local, Global, false},
		{"explicit local with store", "local", true, Both, Local, false},
		{"explicit local without store errors", "local", false, Both, "", true},
		{"explicit LOCAL is case insensitive", "  LOCAL  ", true, Both, Local, false},
		{"explicit global", "global", true, Both, Global, false},
		{"explicit global without local store", "global", false, Both, Global, false},
		{"explicit both with store", "both", true, Global, Both, false},
		{"explicit both degrades without local", "both", false, Global, Global, false},
		{"unknown value errors", "elsewhere", true, Both, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.raw, tc.hasLocal, tc.def)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Normalize(%q, %v, %q) = %q, want error", tc.raw, tc.hasLocal, tc.def, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%q, %v, %q): %v", tc.raw, tc.hasLocal, tc.def, err)
			}
			if got != tc.want {
				t.Fatalf("Normalize(%q, %v, %q) = %q, want %q", tc.raw, tc.hasLocal, tc.def, got, tc.want)
			}
		})
	}
}

// TestTargets_LocalIsListedFirst — merged output leads with the repository's own
// memory, so this ordering is load-bearing for the rendered sections.
func TestTargets_LocalIsListedFirst(t *testing.T) {
	root := t.TempDir()
	if _, err := InitLocal(root); err != nil {
		t.Fatalf("InitLocal: %v", err)
	}
	set := Open(nil, root)
	defer set.Close()

	targets, err := set.Targets(Both)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}
	if targets[0].Location != Local || targets[1].Location != Global {
		t.Fatalf("target order = %q, %q; want local, global", targets[0].Location, targets[1].Location)
	}
}

func TestTargets_BothWithoutLocalIsGlobalOnly(t *testing.T) {
	set := Open(nil, t.TempDir())
	defer set.Close()

	targets, err := set.Targets(Both)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0].Location != Global {
		t.Fatalf("expected global-only targets, got %+v", targets)
	}
}

// TestOpen_FencesLocalIDs ties the store-level fence to the memloc entry point:
// anything opened through Open must already be fenced.
func TestOpen_FencesLocalIDs(t *testing.T) {
	root := t.TempDir()
	if _, err := InitLocal(root); err != nil {
		t.Fatalf("InitLocal: %v", err)
	}
	set := Open(nil, root)
	defer set.Close()

	if err := set.Local.CreateSession("s1", "p", "d"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	id, err := set.Local.AddObservation(storeParams())
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if id <= 1_000_000_000 {
		t.Fatalf("local id %d is not fenced", id)
	}
}

// storeParams builds a minimal observation for fence assertions.
func storeParams() store.AddObservationParams {
	return store.AddObservationParams{SessionID: "s1", Type: "manual", Title: "t", Content: "c"}
}
