package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reverie/internal/memloc"
	"reverie/internal/store"
)

// cmdLocal implements `reverie local <init|status|path>`, the opt-in surface for
// repository-local memory.
func cmdLocal(_ store.Config) {
	sub := ""
	if len(os.Args) > 2 {
		sub = os.Args[2]
	}

	switch sub {
	case "init":
		localInit()
	case "status":
		localStatus()
	case "path":
		localPath()
	default:
		fmt.Fprintln(os.Stderr, "usage: engram local <init|status|path>")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  init    create this repository's local memory store")
		fmt.Fprintln(os.Stderr, "  status  show whether local memory is active, and where")
		fmt.Fprintln(os.Stderr, "  path    print the local memory directory (exit 1 if absent)")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "  init flags:")
		fmt.Fprintln(os.Stderr, "    --default-write=local|global   where unqualified saves go (default: global)")
		exitFunc(1)
	}
}

// requireRepoRoot resolves the repository root or exits with an explanation.
func requireRepoRoot() string {
	root := detectRepoRoot()
	if root == "" {
		fmt.Fprintln(os.Stderr, "engram: cannot determine a repository root for this directory.")
		fmt.Fprintln(os.Stderr, "Run this from inside a git repository. If this directory contains several")
		fmt.Fprintln(os.Stderr, "repositories, cd into the one you want local memory for.")
		exitFunc(1)
	}
	return root
}

func localInit() {
	defaultWrite := ""
	for i := 3; i < len(os.Args); i++ {
		if strings.HasPrefix(os.Args[i], "--default-write=") {
			defaultWrite = strings.TrimPrefix(os.Args[i], "--default-write=")
		}
	}
	switch defaultWrite {
	case "", "local", "global":
	default:
		fmt.Fprintf(os.Stderr, "engram: --default-write must be local or global, got %q\n", defaultWrite)
		exitFunc(1)
	}

	root := requireRepoRoot()
	existed := memloc.HasLocal(root)

	dir, err := memloc.InitLocal(root)
	if err != nil {
		fatal(err)
	}
	if defaultWrite != "" {
		if err := memloc.WriteDefaultWrite(dir, memloc.Location(defaultWrite)); err != nil {
			fatal(err)
		}
	}

	if existed {
		fmt.Printf("Local memory already initialized: %s\n", dir)
	} else {
		fmt.Printf("Local memory initialized: %s\n", dir)
	}

	set := memloc.OpenLocalOnly(root)
	defer set.Close()
	fmt.Printf("  default write target: %s\n", set.DefaultWrite())
	fmt.Printf("  observation ids start above: %d\n", store.LocalIDFence)
	if set.Warning != "" {
		fmt.Printf("  warning: %s\n", set.Warning)
	}
	fmt.Println()
	fmt.Println("Memories saved here stay in this repository. Reads merge local and global.")
	fmt.Printf("The store is a SQLite database — add this to .gitignore unless you intend to commit it:\n")
	fmt.Printf("  %s\n", filepath.ToSlash(filepath.Join(".claude", "engram")+"/"))
}

func localStatus() {
	root := requireRepoRoot()
	dir := memloc.LocalDirFor(root)

	fmt.Printf("repository root: %s\n", root)
	fmt.Printf("local memory:    %s\n", dir)

	if !memloc.HasLocal(root) {
		fmt.Println("status:          not initialized")
		fmt.Println()
		fmt.Println("Run `reverie local init` to create it.")
		return
	}

	set := memloc.OpenLocalOnly(root)
	defer set.Close()
	if !set.HasLocalStore() {
		fmt.Println("status:          present but unreadable")
		if set.Warning != "" {
			fmt.Printf("warning:         %s\n", set.Warning)
		}
		exitFunc(1)
		return
	}

	fmt.Println("status:          active")
	fmt.Printf("default write:   %s\n", set.DefaultWrite())

	stats, err := set.Local.Stats()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read local stats: %v\n", err)
		return
	}
	fmt.Printf("observations:    %d\n", stats.TotalObservations)
	fmt.Printf("sessions:        %d\n", stats.TotalSessions)
	fmt.Printf("id fence:        ids above %d\n", store.LocalIDFence)
	if set.Warning != "" {
		fmt.Printf("warning:         %s\n", set.Warning)
	}
}

func localPath() {
	root := detectRepoRoot()
	if root == "" || !memloc.HasLocal(root) {
		exitFunc(1)
		return
	}
	fmt.Println(memloc.LocalDirFor(root))
}
