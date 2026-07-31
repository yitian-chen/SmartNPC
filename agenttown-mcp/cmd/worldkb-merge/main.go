// Command worldkb-merge synthesizes world.generated.json and
// world.authored.json into world_kb.yaml + world_kb.manifest.json.
//
// Usage:
//
//	worldkb-merge \
//	  --generated assets/world.generated.json \
//	  --authored  assets/world.authored.json \
//	  --out       assets/world_kb.yaml \
//	  --manifest  assets/world_kb.manifest.json
//
// Exit codes: 0 success, 1 validation/merge error, 2 I/O error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

func main() {
	generatedPath := flag.String("generated", "assets/world.generated.json", "path to world.generated.json")
	authoredPath := flag.String("authored", "assets/world.authored.json", "path to world.authored.json")
	outPath := flag.String("out", "assets/world_kb.yaml", "output world_kb.yaml path")
	manifestPath := flag.String("manifest", "assets/world_kb.manifest.json", "output manifest path (empty to skip)")
	verbose := flag.Bool("v", false, "verbose: print warnings to stderr")
	flag.Parse()

	gen, err := loadGenerated(*generatedPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	auth, err := loadAuthored(*authoredPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	kb, warnings, err := worldkb.Merge(gen, auth)
	if err != nil {
		fmt.Fprintln(os.Stderr, "merge error:", err)
		os.Exit(1)
	}

	issues := worldkb.Validate(kb)
	for _, iss := range issues.All() {
		severity := "WARNING"
		if iss.Severity == worldkb.SeverityError {
			severity = "ERROR"
		}
		prefix := severity
		if iss.Entity != "" {
			prefix = severity + " [" + iss.Entity + "]"
		}
		fmt.Fprintln(os.Stderr, prefix, iss.Code+":", iss.Message)
	}
	if issues.HasErrors() {
		fmt.Fprintln(os.Stderr, "validation errors blocked output")
		os.Exit(1)
	}

	mergedBytes, err := worldkb.WriteYAML(kb, *outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "write yaml error:", err)
		os.Exit(2)
	}

	if *manifestPath != "" {
		genBytes, _ := os.ReadFile(*generatedPath)
		authBytes, _ := os.ReadFile(*authoredPath)
		sourceMap := ""
		if gen.Source.MapPackage != "" {
			sourceMap = gen.Source.MapPackage
		}
		if err := worldkb.WriteManifest(genBytes, authBytes, mergedBytes, *manifestPath, sourceMap); err != nil {
			fmt.Fprintln(os.Stderr, "write manifest error:", err)
			os.Exit(2)
		}
	}

	if *verbose {
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "WARNING ["+w.EntityType+"]", w.EntityID+":", w.Message)
		}
	}

	fmt.Printf("ok: %d zones, %d objects, %d agents → %s\n",
		len(kb.Zones), len(kb.Objects), len(kb.Agents), *outPath)
}

func loadGenerated(path string) (*worldkb.GeneratedDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc worldkb.GeneratedDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &doc, nil
}

func loadAuthored(path string) (*worldkb.AuthoredDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc worldkb.AuthoredDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &doc, nil
}
